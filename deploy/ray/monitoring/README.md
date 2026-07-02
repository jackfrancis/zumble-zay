# Ray cluster observability: dashboard + Prometheus + Grafana

This follows the **official KubeRay monitoring guide**
(https://docs.ray.io/en/latest/cluster/kubernetes/k8s-ecosystem/prometheus-grafana.html),
not a hand-rolled Prometheus. It installs the `kube-prometheus-stack` (Prometheus
Operator + Grafana + Alertmanager + kube-state-metrics + node-exporter), applies
KubeRay's `PodMonitor`s and `PrometheusRule`, and auto-loads Ray's Grafana
dashboards.

## Ray dashboard (built-in, no install)

The RayCluster head already serves the Ray dashboard and a Prometheus metrics
endpoint (see `deploy/ray/raycluster.yaml`, service `zz-ray-head-svc`):

```sh
kubectl -n zumble-zay port-forward svc/zz-ray-head-svc 8265:8265   # dashboard
# open http://localhost:8265  (jobs, actors — incl. the llm-rank Scorer actors)

kubectl -n zumble-zay port-forward svc/zz-ray-head-svc 8080:8080   # raw metrics
curl localhost:8080/metrics                                        # ray_* series
```

## Prometheus + Grafana (official KubeRay install)

The KubeRay repo ships the install assets. Clone it and run the installer, then
point the PodMonitors at this project's namespace (`zumble-zay`, not the guide's
`default`):

```sh
git clone --depth 1 https://github.com/ray-project/kuberay.git
cd kuberay

# PodMonitors default to namespace "default"; this project's RayCluster is in
# "zumble-zay", so retarget them before the installer applies them.
sed -i '' 's/      - default/      - zumble-zay/' config/prometheus/podMonitor.yaml

# Installs kube-prometheus-stack v48.2.1 into namespace prometheus-system,
# applies config/prometheus/podMonitor.yaml + rules, and auto-loads Ray's
# Grafana dashboards.
./install/prometheus/install.sh --auto-load-dashboard true
```

The installer creates two `PodMonitor`s — `ray-head-monitor` and
`ray-workers-monitor` — which scrape the Ray pods' `:8080/metrics`, and a
`PrometheusRule` (`ray-cluster-gcs-rules`).

### Access

```sh
# Prometheus
kubectl -n prometheus-system port-forward svc/prometheus-kube-prometheus-prometheus 9090:9090
# http://localhost:9090 — try:  ray_component_cpu_percentage  or  ray_tasks

# Grafana (user: admin; password below)
kubectl -n prometheus-system get secret prometheus-grafana -o jsonpath='{.data.admin-password}' | base64 -d; echo
kubectl -n prometheus-system port-forward svc/prometheus-grafana 3000:80
# http://localhost:3000 — dashboards: "Ray", "Serve", "Serve LLM", "Train",
# "Data", "KubeRay Operator", ...
```

### Verify the Ray targets are up

```sh
# Both should show health "up":
curl -s localhost:9090/api/v1/targets \
  | python3 -c 'import sys,json;[print(t["scrapePool"],t["health"]) for t in json.load(sys.stdin)["data"]["activeTargets"] if "ray" in t["scrapePool"]]'
```

## Notes

- This is a **heavy** footprint (Grafana, operator, alertmanager, node-exporter,
  kube-state-metrics). On the local podman VM, ensure it has enough memory
  (`podman machine set --memory 12288`).
- Per-actor series are visible: the llm-rank actors path (docs/adr/0029) shows up
  as `ray::Scorer.score` components in `ray_component_*` metrics.
- Teardown: `helm -n prometheus-system uninstall prometheus` and
  `kubectl delete ns prometheus-system`.

## Custom application metrics (actors llm-rank)

Beyond Ray's built-in system metrics, the actors path emits its own
**application metrics** via `ray.util.metrics` (in `deploy/ray/llm_rank_ray.py`).
Ray exports them on the same `:8080/metrics` endpoint the PodMonitors scrape, so
they need no extra config — Ray prefixes them with `ray_`:

| Metric | Type | Tags | Meaning |
|--------|------|------|---------|
| `ray_zz_items_scored` | counter | `model` | Items successfully scored |
| `ray_zz_score_errors` | counter | `model`, `kind` | Failures by kind (`http_401`, `parse_or_network`, `no_token`) |
| `ray_zz_score_latency_seconds` | histogram | `model` | Per-item Copilot call latency |

Example PromQL:

```promql
sum(ray_zz_items_scored)                                     # total scored
sum by (kind) (ray_zz_score_errors)                          # failure breakdown
histogram_quantile(0.95,
  sum(rate(ray_zz_score_latency_seconds_bucket[5m])) by (le)) # p95 latency
```

**Batch-job caveat.** These actors are short-lived, so their metrics can be gone
before Ray's metric agent exports them and Prometheus (15s interval) scrapes
them. The job therefore supports an optional linger — set
`RAY_LLM_RANK_METRICS_LINGER_S` on the orchestrator (e.g. `45`) so it holds after
scoring long enough for the metrics to land. Default `0` (no linger) for
production, where a proper batch-metrics sink (e.g. Prometheus Pushgateway) is
the real answer.

