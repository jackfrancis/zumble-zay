#!/usr/bin/env python3
"""Ray-actors execution of the llm-rank job (docs/adr/0031).

This is how the ray substrate runs llm-rank. Where every *other* job type runs
/runtime (a single Go process) as the RayJob entrypoint, an llm-rank job runs
THIS program ON the Ray cluster and fans the per-item Copilot scoring out across
a pool of Ray *actors* — genuine @ray.remote workers spread across the
RayCluster's head and worker nodes — then gathers the results and writes the
proposals back to ZZ.

It speaks the exact same ZZ agent HTTP contract as internal/agent (the Go
runtime), so ZZ core is unchanged:
  * GET  {ZZ_BASE_URL}/agent/worklist            -> {"items": [WorkItem, ...]}
  * POST {ZZ_BASE_URL}/agent/worklist  {"items"} -> writes items back
Each item's Signals.Proposed (JSON "signals.proposed") is set to the LLM axes;
ZZ ratifies them against its deterministic baseline (docs/adr/0011), exactly as
for the Go path.

Config comes from the same ZZ_* injection contract (docs/adr/0012):
  ZZ_BASE_URL, ZZ_JOB_TOKEN                  (per-job, via runtimeEnvYAML)
  ZZ_AI_ENDPOINT, ZZ_AI_MODEL, ZZ_AI_TOKEN   (model; AI_TOKEN carried by the
                                              cluster pods, never in the CR)

Run (as a RayJob entrypoint): `python /llm_rank_ray.py`
"""
import json
import os
import sys
import time
import urllib.error
import urllib.request

import ray
from ray.util.metrics import Counter, Histogram

# --- config from the ZZ_* injection contract (docs/adr/0012) ---
ZZ_BASE_URL = os.environ["ZZ_BASE_URL"].rstrip("/")
ZZ_JOB_TOKEN = os.environ["ZZ_JOB_TOKEN"]
AI_ENDPOINT = os.environ.get("ZZ_AI_ENDPOINT", "https://api.githubcopilot.com/chat/completions")
AI_MODEL = os.environ.get("ZZ_AI_MODEL", "claude-opus-4.8")
# The model token is read per-actor from ZZ_AI_TOKEN on the actor's own node
# (see Scorer.__init__); it is intentionally not read here at driver scope.
# Public, non-secret integration id required by the Copilot endpoint; ignored by
# other OpenAI-compatible providers (mirrors internal/llm/ranker.go).
COPILOT_INTEGRATION_ID = "copilot-developer-cli"
# Number of scoring actors to spread across the cluster. Kept modest for a small
# kind RayCluster; a real cluster would scale this with worker count.
NUM_SCORERS = int(os.environ.get("RAY_LLM_RANK_ACTORS_N", "4"))

SYSTEM_PROMPT = (
    'You rank a software engineer\'s GitHub work items for a personal '
    '"what needs my attention" radar.\n\n'
    "Score the item on four axes, each a number from 0.0 to 1.0:\n"
    "- relevance: how much this item needs the user's OWN attention right now.\n"
    "- impact: how consequential the underlying change is.\n"
    "- engagement: how much active collaboration is happening.\n"
    "- urgency: how time-sensitive it is FOR THE USER.\n\n"
    "Also return:\n"
    "- confidence: 0.0 to 1.0, how sure you are given the limited signals.\n"
    "- rationale: one short sentence, addressed to the reader in the SECOND "
    'PERSON ("you"/"your").\n\n'
    "Respond with ONLY a JSON object, no prose, with exactly these keys:\n"
    '{"relevance":0.0,"impact":0.0,"engagement":0.0,"urgency":0.0,'
    '"confidence":0.0,"rationale":"..."}'
)


def _clamp01(x):
    try:
        x = float(x)
    except (TypeError, ValueError):
        return 0.0
    return max(0.0, min(1.0, x))


def _item_summary(item):
    """Compact signal summary for the model (mirrors llm.userPrompt)."""
    gh = item.get("github", {})
    sig = item.get("signals", {})
    return {
        "repo": gh.get("repo"),
        "type": item.get("type"),
        "title": gh.get("title"),
        "state": gh.get("state"),
        "reasons": sig.get("reasons", []),
        "labels": sig.get("labels", []),
        "comments": sig.get("comments", 0),
        "participants": sig.get("participants", 0),
        "reactions": sig.get("reactions", 0),
        "inbound_refs": sig.get("inbound_refs", 0),
    }


@ray.remote
class Scorer:
    """A Ray actor that scores one item at a time via the Copilot endpoint.

    Each actor holds its own HTTP setup and runs on whatever node Ray schedules
    it onto, so a pool of these scores the worklist in parallel ACROSS the
    cluster — the intra-job parallelism the Go /runtime path does not have.

    The model token is read from THIS actor's own node env (ZZ_AI_TOKEN), which
    every RayCluster pod carries (docs/adr/0028, 0029) — so the token never has
    to travel through the driver or the per-job CR.
    """

    def __init__(self, endpoint, model, integration_id):
        self.endpoint = endpoint
        self.model = model
        self.integration_id = integration_id
        self.token = os.environ.get("ZZ_AI_TOKEN", "")
        # Application metrics (docs/adr/0031): exported through the same Ray
        # metrics endpoint the KubeRay PodMonitors already scrape, so they land in
        # Prometheus as zz_items_scored_total / zz_score_errors_total /
        # zz_score_latency_seconds with no extra scrape config. Defined per actor;
        # Prometheus aggregates by name+tags across the pool. The "model" tag lets
        # a dashboard break results down per ranking model.
        self._m_scored = Counter(
            "zz_items_scored",
            description="Work items successfully scored by the actors llm-rank path.",
            tag_keys=("model",),
        )
        self._m_errors = Counter(
            "zz_score_errors",
            description="Per-item scoring failures, tagged by kind.",
            tag_keys=("model", "kind"),
        )
        self._m_latency = Histogram(
            "zz_score_latency_seconds",
            description="Per-item Copilot scoring call latency (seconds).",
            boundaries=[0.25, 0.5, 1, 2, 4, 8, 16, 32, 60],
            tag_keys=("model",),
        )

    def score(self, item):
        """Return (item_id, proposal_dict) or (item_id, None) on any failure.

        Best-effort per item, mirroring the Go runtime: a failed proposal leaves
        the item unchanged rather than failing the whole job.
        """
        item_id = item.get("id")
        tags = {"model": self.model}
        if not self.token:
            print(f"scorer: item {item_id} skipped: no ZZ_AI_TOKEN on this node",
                  file=sys.stderr)
            self._m_errors.inc(1, tags={**tags, "kind": "no_token"})
            return item_id, None
        body = json.dumps(
            {
                "model": self.model,
                "messages": [
                    {"role": "system", "content": SYSTEM_PROMPT},
                    {"role": "user", "content": "Score this GitHub work item:\n"
                     + json.dumps(_item_summary(item))},
                ],
                "temperature": 0,
                "response_format": {"type": "json_object"},
            }
        ).encode()
        req = urllib.request.Request(self.endpoint, data=body, method="POST")
        req.add_header("Content-Type", "application/json")
        req.add_header("Accept", "application/json")
        req.add_header("Authorization", "Bearer " + self.token)
        req.add_header("Copilot-Integration-Id", self.integration_id)
        started = time.monotonic()
        try:
            with urllib.request.urlopen(req, timeout=60) as resp:
                data = json.loads(resp.read())
            content = data["choices"][0]["message"]["content"].strip()
            if content.startswith("```"):
                content = content.strip("`")
                content = content[content.find("{"):content.rfind("}") + 1]
            doc = json.loads(content)
        except urllib.error.HTTPError as exc:
            print(f"scorer: item {item_id} failed: HTTP {exc.code}", file=sys.stderr)
            self._m_errors.inc(1, tags={**tags, "kind": f"http_{exc.code}"})
            return item_id, None
        except Exception as exc:  # noqa: BLE001 - best-effort per item
            print(f"scorer: item {item_id} failed: {exc}", file=sys.stderr)
            self._m_errors.inc(1, tags={**tags, "kind": "parse_or_network"})
            return item_id, None
        # Record latency and a successful score only on the happy path.
        self._m_latency.observe(time.monotonic() - started, tags=tags)
        self._m_scored.inc(1, tags=tags)
        return item_id, {
            "relevance": _clamp01(doc.get("relevance")),
            "impact": _clamp01(doc.get("impact")),
            "engagement": _clamp01(doc.get("engagement")),
            "urgency": _clamp01(doc.get("urgency")),
            "confidence": _clamp01(doc.get("confidence")),
            "rationale": str(doc.get("rationale", "")).strip(),
        }


def _zz_get(path):
    req = urllib.request.Request(ZZ_BASE_URL + path, method="GET")
    req.add_header("Authorization", "Bearer " + ZZ_JOB_TOKEN)
    with urllib.request.urlopen(req, timeout=30) as resp:
        return json.loads(resp.read())


def _zz_post(path, payload):
    req = urllib.request.Request(
        ZZ_BASE_URL + path, data=json.dumps(payload).encode(), method="POST"
    )
    req.add_header("Authorization", "Bearer " + ZZ_JOB_TOKEN)
    req.add_header("Content-Type", "application/json")
    with urllib.request.urlopen(req, timeout=30) as resp:
        return resp.status


def main():
    # Join the standing RayCluster this entrypoint runs on. The model token is NOT
    # required by the driver — each Scorer actor reads ZZ_AI_TOKEN from its own
    # node env (docs/adr/0031) — so the driver only needs the ZZ contract.
    ray.init(address="auto")

    items = _zz_get("/agent/worklist").get("items", [])
    if not items:
        print("llm_rank_ray: no items to rank.")
        return

    # Spin up a pool of scoring actors spread across the cluster, then round-robin
    # the items across them and gather the proposals in parallel.
    n = min(NUM_SCORERS, len(items))
    scorers = [
        Scorer.remote(AI_ENDPOINT, AI_MODEL, COPILOT_INTEGRATION_ID)
        for _ in range(n)
    ]
    futures = [scorers[i % n].score.remote(item) for i, item in enumerate(items)]
    results = ray.get(futures)

    proposals = {item_id: prop for item_id, prop in results if prop is not None}

    # Attach each proposal to its item's signals.proposed and write back ONLY the
    # items that got a proposal (best-effort, mirroring the Go runtime).
    changed = []
    for item in items:
        prop = proposals.get(item.get("id"))
        if prop is None:
            continue
        item.setdefault("signals", {})["proposed"] = prop
        changed.append(item)

    if changed:
        _zz_post("/agent/worklist", {"items": changed})
    print(f"llm_rank_ray: scored {len(changed)}/{len(items)} items via "
          f"{n} Ray actors.")

    # Application metrics from short-lived actors are only useful if the node's
    # metrics agent gets to export them and Prometheus gets to scrape them before
    # the job tears down. Optionally linger (keeping the actor handles alive) for
    # a couple of scrape intervals so zz_items_scored/zz_score_errors/
    # zz_score_latency_seconds land in Prometheus (docs/adr/0031). Default 0 (no
    # linger) for production; set RAY_LLM_RANK_METRICS_LINGER_S to enable.
    linger = int(os.environ.get("RAY_LLM_RANK_METRICS_LINGER_S", "0"))
    if linger > 0:
        print(f"llm_rank_ray: lingering {linger}s so metrics are scrapeable...")
        time.sleep(linger)
    # Reference scorers after the linger so they are not GC'd early.
    del scorers


if __name__ == "__main__":
    main()
