.PHONY: build run mint-keys test tidy vet fmt clean image image-save kind-load engine \
        cluster-up cluster-down dev-up dev-down dev-forward dev-logs dev-logs-orchestrator \
        build-runtime image-runtime image-runtime-save kind-load-runtime \
        build-orchestrator image-orchestrator image-orchestrator-save kind-load-orchestrator vendor-primer \
        image-runtime-shell image-runtime-shell-save kind-load-runtime-shell opensandbox-install \
        image-runtime-a2a image-runtime-a2a-save kind-load-runtime-a2a kagent-install \
        substrate-install substrate-cluster

BIN := bin/server

# Container engine autodetection: prefer podman (common on macOS), fall back to
# docker. Override explicitly with `make image CONTAINER_ENGINE=docker`.
CONTAINER_ENGINE ?= $(shell command -v podman 2>/dev/null || command -v docker 2>/dev/null)
# localhost-qualified so the name is identical under podman and docker (podman
# auto-prefixes unqualified names with localhost/). Kubernetes treats the
# localhost registry as local, so kind-loaded images resolve without a pull.
IMAGE ?= localhost/zumble-zay:dev

# Standalone agent runtime artifact (docs/adr/0012): the same runtime that runs
# in-process, built as a binary/image so it can run as an out-of-process workload.
RUNTIME_BIN := bin/runtime
RUNTIME_IMAGE ?= localhost/zumble-zay-runtime:dev

# Orchestrator control-plane artifact (docs/adr/0023): the agent control plane,
# extracted from the web tier so Pod/Job-creation privilege lives only here.
ORCHESTRATOR_BIN := bin/orchestrator
ORCHESTRATOR_IMAGE ?= localhost/zumble-zay-orchestrator:dev

# Agent-runtime substrate selected for the dev cluster (docs/adr/0012). It drives
# the whole `make dev-up` cycle: with LAUNCHER=agent-sandbox (ADR 0026) the
# orchestrator image is built with the agent_sandbox build tag, the agent-sandbox
# controller + CRDs are installed, and the orchestrator is deployed with it
# selected. Defaults to the deployed default, so a bare `make dev-up` is unchanged.
LAUNCHER ?= k8s-job
# Optional build-tagged substrates compile into the orchestrator image only when
# selected. Each adds its Go build tag on its OWN line via += — so a new substrate
# (e.g. a future ray/kuberay or opensandbox) appends a line rather than editing a
# shared one, keeping concurrent launcher work merge-clean. ORCHESTRATOR_GO_TAGS is
# recursively expanded and $(strip)s the accumulated list (empty when no tagged
# substrate is selected), so the += lines below may be extended in any order. The
# Go-identifier tag (agent_sandbox) is the underscore form of the hyphenated
# LAUNCHER value (agent-sandbox).
ORCHESTRATOR_GO_TAGS = $(strip $(ORCHESTRATOR_GO_TAGS_LIST))
ORCHESTRATOR_GO_TAGS_LIST += $(if $(filter agent-sandbox,$(LAUNCHER)),agent_sandbox,)
# Pinned agent-sandbox release for the optional controller + CRDs install.
AGENT_SANDBOX_VERSION ?= v0.5.0

# Shell-bearing runtime image for the OpenSandbox substrate (docs/adr/0027): the
# same /runtime binary on a base with a shell + coreutils, since OpenSandbox runs
# the runtime via `sh -c` inside a keep-alive container (the distroless runtime
# image cannot run there).
RUNTIME_SHELL_IMAGE ?= localhost/zumble-zay-runtime-shell:dev
# OpenSandbox (LAUNCHER=opensandbox) dev-install knobs — all overridable. The
# server Helm chart installs from local source, so a pinned checkout is cloned
# under build/. EXPERIMENTAL: this stands up the full OpenSandbox platform
# (controller + server, default published images the cluster pulls) and is not yet
# validated end-to-end; expect to tune image pulls / the batchsandbox template per
# cluster. helm + git are required.
# OPENSANDBOX_REF pins the SERVER chart source to the release matching the server
# image (main's charts can drift ahead of the last published images, passing flags
# the published binary rejects). The CONTROLLER is installed from its published
# chart release (self-consistent with its image), not the source chart, to avoid
# that skew.
OPENSANDBOX_REF ?= server/v0.2.1
OPENSANDBOX_CONTROLLER_CHART_VERSION ?= 0.2.0
OPENSANDBOX_NAMESPACE ?= opensandbox-system
OPENSANDBOX_API_KEY ?= zumble-zay-dev
OPENSANDBOX_EXECD_IMAGE ?= sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/execd:v1.0.20
OPENSANDBOX_EGRESS_IMAGE ?= sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/egress:v1.1.3
OPENSANDBOX_CLONE_DIR := build/opensandbox

# Runtime image for the kagent substrate (docs/adr/0024): the cmd/runtime-a2a
# server (the ZZ runtime behind an A2A endpoint), run by kagent as a durable BYO
# Agent. Unlike the pod runtimes it is a long-running server, not a one-shot job.
RUNTIME_A2A_IMAGE ?= localhost/zumble-zay-runtime-a2a:dev
# kagent (LAUNCHER=kagent) dev-install knobs — all overridable. kagent installs
# from its published OCI Helm charts; the controller's model provider is a dummy
# because the ZZ BYO agent routes its model calls through the agentgateway
# (ZZ_AI_ENDPOINT), not kagent's own ModelConfig.
KAGENT_VERSION ?= 0.9.11
KAGENT_NAMESPACE ?= kagent

# Agent Substrate (LAUNCHER=substrate) dev knobs — all overridable (docs/adr/0035).
# EXPERIMENTAL: Substrate is v0.0.0. `LAUNCHER=substrate make dev-up` bootstraps the
# whole thing — cluster-up resolves to substrate-cluster, which clones Substrate at
# SUBSTRATE_REF and runs ITS gated-kind + ate-system installer (a feature-gated
# cluster is required, and its images are ko-built, so there is nothing to `kubectl
# apply`). The dev-up substrate branch then pushes the runtime-a2a actor image to the
# in-cluster registry as a digest, ko-resolves the ateom herder, and substrate-install
# applies ZZ's pool/template/actor against that ate-system. The actor reuses the
# cmd/runtime-a2a image, served on :80. Cross-namespace FQDNs because the actor runs
# in ate-land, not the web tier's namespace (cf. opensandbox, ADR 0027).
SUBSTRATE_REF ?= main
SUBSTRATE_CLONE_DIR := build/substrate
SUBSTRATE_REGISTRY ?= localhost:5001
SUBSTRATE_NAMESPACE ?= zumble-zay-substrate
SUBSTRATE_ATESPACE ?= zumble-zay
SUBSTRATE_ACTOR ?= zz-runtime
SUBSTRATE_TEMPLATE ?= $(SUBSTRATE_NAMESPACE)/zz-runtime
SUBSTRATE_WORKER_REPLICAS ?= 2
# The gVisor "ateom" herder image your ate-system installed; override to match it
# (a bare ko:// ref is only resolvable by Substrate's own tooling, not kubectl).
SUBSTRATE_ATEOM_IMAGE ?= ko://github.com/agent-substrate/substrate/cmd/ateom-gvisor
SUBSTRATE_PAUSE_IMAGE ?= registry.k8s.io/pause:3.10.2@sha256:f548e0e8e3dc1896ca956272154dde3314e8cc4fde0a57577ee9fa1c63f5baf4
# MUST be a cluster-pullable digest (name@sha256:…) — the ActorTemplate rejects
# unpinned images, so the kind-loaded :dev tag does not qualify.
SUBSTRATE_RUNTIME_IMAGE ?= $(RUNTIME_A2A_IMAGE)
SUBSTRATE_SNAPSHOT_LOCATION ?= gs://REPLACE-ME/zz-runtime/
SUBSTRATE_ROUTER_URL ?= http://atenet-router.ate-system.svc.cluster.local
SUBSTRATE_ZZ_BASE_URL ?= http://zumble-zay.$(KUBE_NS).svc.cluster.local:8080
SUBSTRATE_ZZ_AI_ENDPOINT ?= http://zumble-zay-agentgateway.$(KUBE_NS).svc.cluster.local/chat/completions
SUBSTRATE_ZZ_AI_MODEL ?= claude-opus-4.8

# Vendored GitHub Primer CSS (the UI's design system, served from the binary).
# `make vendor-primer` refreshes these at the pinned versions; bump the versions
# and re-run to scoop in updates.
PRIMER_CSS_VERSION ?= 22.3.0
PRIMER_PRIMITIVES_VERSION ?= 11.9.0
PRIMER_DIR := internal/webui/static/primer

KIND_CLUSTER ?= zumble-zay
KUBE_NS := zumble-zay

# The base kustomization ships a default-deny NetworkPolicy on the orchestrator's
# control API (docs/adr/0033). kind's default CNI (kindnet) does NOT enforce
# NetworkPolicy, so dev-up installs the kube-network-policies controller (a
# kubernetes-sigs DaemonSet that enforces standard NetworkPolicy via nftables) to
# make the policy live in dev. Pinned + overridable; set to empty to skip the
# install (the policy then stays inert/fail-open, exactly as on bare kindnet).
KUBE_NETWORK_POLICIES_VERSION ?= v1.1.0

# kind must use the podman provider when podman is the chosen engine.
ifeq ($(findstring podman,$(CONTAINER_ENGINE)),podman)
export KIND_EXPERIMENTAL_PROVIDER = podman
endif

build:
	go build -o $(BIN) ./cmd/server

# Build the standalone agent runtime binary.
build-runtime:
	go build -o $(RUNTIME_BIN) ./cmd/runtime

# Build the orchestrator control-plane binary (docs/adr/0023).
build-orchestrator:
	go build -o $(ORCHESTRATOR_BIN) ./cmd/orchestrator

run:
	go run ./cmd/server

# Generate a fresh Ed25519 job-token keypair for true issuer/verifier separation
# in a split deployment (docs/adr/0023): MINT_PRIVATE_KEY goes to the orchestrator
# (a secret; the sole signer) and MINT_PUBLIC_KEY to the web tier (config; verify
# only). Without it both tiers derive the pair from SESSION_SECRET, which also
# lets the web tier mint.
mint-keys:
	@go run ./cmd/mint-keygen

test:
	go test ./...

tidy:
	go mod tidy

vet:
	go vet ./...

fmt:
	gofmt -l -w .

clean:
	rm -rf bin zumble-zay-image.tar zumble-zay-runtime-image.tar zumble-zay-orchestrator-image.tar

# Refresh the vendored GitHub Primer CSS at the pinned versions (the UI's design
# system). Re-run after bumping PRIMER_*_VERSION to scoop in updates.
vendor-primer:
	@mkdir -p $(PRIMER_DIR)
	curl -sfL https://unpkg.com/@primer/css@$(PRIMER_CSS_VERSION)/dist/primer.css -o $(PRIMER_DIR)/primer.css
	@for f in primitives.css \
		base/motion/motion.css base/size/size.css base/size/z-index.css base/typography/typography.css \
		functional/motion/motion.css functional/size/border.css functional/size/breakpoints.css \
		functional/size/radius.css functional/size/size-coarse.css functional/size/size-fine.css \
		functional/size/size.css functional/size/z-index.css functional/spacing/space.css \
		functional/typography/typography.css functional/themes/light.css; do \
		mkdir -p "$(PRIMER_DIR)/$$(dirname $$f)"; \
		curl -sfL "https://unpkg.com/@primer/primitives@$(PRIMER_PRIMITIVES_VERSION)/dist/css/$$f" -o "$(PRIMER_DIR)/$$f" || exit 1; \
	done
	@echo "vendored Primer CSS $(PRIMER_CSS_VERSION) + primitives $(PRIMER_PRIMITIVES_VERSION) into $(PRIMER_DIR)"

# Print the detected container engine.
engine:
	@test -n "$(CONTAINER_ENGINE)" || { echo "no container engine found (install podman or docker)"; exit 1; }
	@echo "using container engine: $(CONTAINER_ENGINE)"

# Build the image with whichever engine is available.
image: engine
	$(CONTAINER_ENGINE) build -t $(IMAGE) .

# Build the standalone agent runtime image (docs/adr/0012).
image-runtime: engine
	$(CONTAINER_ENGINE) build -f Dockerfile.runtime -t $(RUNTIME_IMAGE) .

# Build the orchestrator control-plane image (docs/adr/0023). GO_TAGS compiles in
# build-tagged substrates when selected (empty by default, so the image is
# unchanged unless LAUNCHER=agent-sandbox).
image-orchestrator: engine
	$(CONTAINER_ENGINE) build -f Dockerfile.orchestrator --build-arg GO_TAGS=$(ORCHESTRATOR_GO_TAGS) -t $(ORCHESTRATOR_IMAGE) .

# Export the image to a portable archive (works with docker and podman).
image-save: image
	$(CONTAINER_ENGINE) save $(IMAGE) -o zumble-zay-image.tar

# Load the image into a local kind cluster via the engine-agnostic archive path
# (kind's `load docker-image` is docker-only; `load image-archive` is not).
kind-load: image-save
	kind load image-archive zumble-zay-image.tar --name $(KIND_CLUSTER)
	rm -f zumble-zay-image.tar

# Export the runtime image to a portable archive (docs/adr/0012).
image-runtime-save: image-runtime
	$(CONTAINER_ENGINE) save $(RUNTIME_IMAGE) -o zumble-zay-runtime-image.tar

# Load the standalone runtime image into kind so KubernetesJobLauncher Jobs
# resolve it without a registry pull (same engine-agnostic archive path as
# kind-load). Run this before exercising LAUNCHER=k8s-job (docs/adr/0012).
kind-load-runtime: image-runtime-save
	kind load image-archive zumble-zay-runtime-image.tar --name $(KIND_CLUSTER)
	rm -f zumble-zay-runtime-image.tar

# Build the shell-bearing runtime image for the OpenSandbox substrate (docs/adr/0027).
image-runtime-shell: engine
	$(CONTAINER_ENGINE) build -f Dockerfile.runtime-shell -t $(RUNTIME_SHELL_IMAGE) .

# Export the shell runtime image to a portable archive.
image-runtime-shell-save: image-runtime-shell
	$(CONTAINER_ENGINE) save $(RUNTIME_SHELL_IMAGE) -o zumble-zay-runtime-shell-image.tar

# Load the shell runtime image into kind so OpenSandbox sandboxes resolve it
# without a registry pull. Run before exercising LAUNCHER=opensandbox (docs/adr/0027).
kind-load-runtime-shell: image-runtime-shell-save
	kind load image-archive zumble-zay-runtime-shell-image.tar --name $(KIND_CLUSTER)
	rm -f zumble-zay-runtime-shell-image.tar

# Build the A2A runtime-server image for the kagent substrate (docs/adr/0024).
image-runtime-a2a: engine
	$(CONTAINER_ENGINE) build -f Dockerfile.runtime-a2a -t $(RUNTIME_A2A_IMAGE) .

# Export the A2A runtime image to a portable archive.
image-runtime-a2a-save: image-runtime-a2a
	$(CONTAINER_ENGINE) save $(RUNTIME_A2A_IMAGE) -o zumble-zay-runtime-a2a-image.tar

# Load the A2A runtime image into kind so the kagent BYO Agent resolves it without
# a registry pull. Run before exercising LAUNCHER=kagent (docs/adr/0024).
kind-load-runtime-a2a: image-runtime-a2a-save
	kind load image-archive zumble-zay-runtime-a2a-image.tar --name $(KIND_CLUSTER)
	rm -f zumble-zay-runtime-a2a-image.tar

# EXPERIMENTAL (docs/adr/0027): install the OpenSandbox platform into the kind
# cluster so LAUNCHER=opensandbox has a control plane to drive. Installs the
# controller (CRDs + operator) from its PUBLISHED chart release (self-consistent
# with its image) and the lifecycle server from a pinned source checkout (the
# server chart is local-source-only), configured for the Kubernetes runtime +
# batchsandbox provider with a dev API key, using the charts' default published
# images (the cluster pulls them). Not yet validated end-to-end; the OPENSANDBOX_*
# knobs above are overridable.
opensandbox-install:
	@command -v helm >/dev/null || { echo "helm not installed (https://helm.sh)"; exit 1; }
	rm -rf $(OPENSANDBOX_CLONE_DIR)
	git clone --depth 1 --branch $(OPENSANDBOX_REF) https://github.com/opensandbox-group/OpenSandbox $(OPENSANDBOX_CLONE_DIR)
	@kubectl create namespace $(OPENSANDBOX_NAMESPACE) --dry-run=client -o yaml | kubectl apply -f -
	# Controller from its PUBLISHED chart release (self-consistent with its image);
	# the source chart on a moving ref can pass flags the published binary lacks.
	helm upgrade --install opensandbox-controller \
		https://github.com/opensandbox-group/OpenSandbox/releases/download/helm/opensandbox-controller/$(OPENSANDBOX_CONTROLLER_CHART_VERSION)/opensandbox-controller-$(OPENSANDBOX_CONTROLLER_CHART_VERSION).tgz \
		--namespace $(OPENSANDBOX_NAMESPACE) --wait --timeout 300s
	@printf 'server:\n  replicaCount: 1\nconfigToml: |\n  [server]\n  host = "0.0.0.0"\n  port = 80\n  api_key = "%s"\n  [log]\n  level = "INFO"\n  [runtime]\n  type = "kubernetes"\n  execd_image = "%s"\n  [kubernetes]\n  namespace = "%s"\n  workload_provider = "batchsandbox"\n  batchsandbox_template_file = "/etc/opensandbox/example.batchsandbox-template.yaml"\n  [egress]\n  image = "%s"\n  mode = "dns+nft"\n' \
		"$(OPENSANDBOX_API_KEY)" "$(OPENSANDBOX_EXECD_IMAGE)" "$(OPENSANDBOX_NAMESPACE)" "$(OPENSANDBOX_EGRESS_IMAGE)" > $(OPENSANDBOX_CLONE_DIR)/zz-server-values.yaml
	helm upgrade --install opensandbox-server $(OPENSANDBOX_CLONE_DIR)/kubernetes/charts/opensandbox-server \
		--namespace $(OPENSANDBOX_NAMESPACE) -f $(OPENSANDBOX_CLONE_DIR)/zz-server-values.yaml --wait --timeout 300s

# Install kagent into the kind cluster so LAUNCHER=kagent has a control plane to
# dispatch to (docs/adr/0024): the published CRDs + controller Helm charts (with a
# dummy model provider — the ZZ BYO agent routes model calls through the
# agentgateway, not kagent's ModelConfig). The zz-runtime BYO Agent itself is
# applied by dev-up AFTER the overlay creates the zumble-zay namespace it lives in
# (the kagent controller reconciles Agent CRs cluster-wide), reconciled into the
# durable Deployment the orchestrator dispatches to.
kagent-install:
	@command -v helm >/dev/null || { echo "helm not installed (https://helm.sh)"; exit 1; }
	helm upgrade --install kagent-crds oci://ghcr.io/kagent-dev/kagent/helm/kagent-crds \
		--version $(KAGENT_VERSION) --namespace $(KAGENT_NAMESPACE) --create-namespace --wait --timeout 300s
	helm upgrade --install kagent oci://ghcr.io/kagent-dev/kagent/helm/kagent \
		--version $(KAGENT_VERSION) --namespace $(KAGENT_NAMESPACE) \
		--set providers.default=openAI --set providers.openAI.apiKey=sk-kagent-dev-unused \
		--wait --timeout 360s

# EXPERIMENTAL (docs/adr/0035): apply the ZZ runtime as a durable Agent Substrate
# actor into an ALREADY-RUNNING ate-system, so LAUNCHER=substrate has an actor to
# dispatch to. This does NOT install ate-system itself (gVisor + object store +
# control plane) — stand that up first with Substrate's own tooling (e.g.
# hack/install-ate-kind.sh --deploy-ate-system from github.com/agent-substrate/
# substrate). It substitutes the SUBSTRATE_* knobs into the pool/template template,
# applies it, waits for the golden snapshot, and creates the atespace + actor via
# kubectl-ate. The runtime image must be a cluster-pullable digest and the snapshot
# location your object store — both overridable knobs above.
substrate-install:
	@command -v kubectl >/dev/null || { echo "kubectl not installed"; exit 1; }
	@case "$(SUBSTRATE_SNAPSHOT_LOCATION)" in *REPLACE-ME*) \
		echo "set SUBSTRATE_SNAPSHOT_LOCATION to your ate-system object store (e.g. gs://bucket/zz/)"; exit 1;; esac
	@case "$(SUBSTRATE_RUNTIME_IMAGE)" in *@sha256:*) ;; *) \
		echo "WARNING: SUBSTRATE_RUNTIME_IMAGE=$(SUBSTRATE_RUNTIME_IMAGE) is not pinned by digest;"; \
		echo "         the ActorTemplate CRD requires name@sha256:… (snapshot immutability).";; esac
	@kubectl get -n ate-system svc/atenet-router >/dev/null 2>&1 || { \
		echo "no svc/atenet-router in ate-system — stand up Agent Substrate first"; \
		echo "(see https://github.com/agent-substrate/substrate)"; exit 1; }
	sed -e 's|$${SUBSTRATE_NAMESPACE}|$(SUBSTRATE_NAMESPACE)|g' \
	    -e 's|$${WORKER_REPLICAS}|$(SUBSTRATE_WORKER_REPLICAS)|g' \
	    -e 's|$${ATEOM_IMAGE}|$(SUBSTRATE_ATEOM_IMAGE)|g' \
	    -e 's|$${RUNTIME_A2A_IMAGE}|$(SUBSTRATE_RUNTIME_IMAGE)|g' \
	    -e 's|$${PAUSE_IMAGE}|$(SUBSTRATE_PAUSE_IMAGE)|g' \
	    -e 's|$${SNAPSHOT_LOCATION}|$(SUBSTRATE_SNAPSHOT_LOCATION)|g' \
	    -e 's|$${ZZ_BASE_URL}|$(SUBSTRATE_ZZ_BASE_URL)|g' \
	    -e 's|$${ZZ_AI_ENDPOINT}|$(SUBSTRATE_ZZ_AI_ENDPOINT)|g' \
	    -e 's|$${ZZ_AI_MODEL}|$(SUBSTRATE_ZZ_AI_MODEL)|g' \
	    deploy/k8s/substrate/zz-runtime.yaml.tmpl | kubectl apply -f -
	@echo "waiting for the ActorTemplate golden snapshot to be Ready (can take minutes)"
	kubectl wait --for=condition=Ready actortemplate/zz-runtime -n $(SUBSTRATE_NAMESPACE) --timeout=600s || true
	@if command -v kubectl-ate >/dev/null; then \
		kubectl ate create atespace $(SUBSTRATE_ATESPACE) || true; \
		kubectl ate create actor $(SUBSTRATE_ACTOR) -a $(SUBSTRATE_ATESPACE) --template=$(SUBSTRATE_TEMPLATE) || true; \
	else \
		echo "kubectl-ate not on PATH; install it and create the actor manually:"; \
		echo "  go install github.com/agent-substrate/substrate/cmd/kubectl-ate@latest"; \
		echo "  kubectl ate create atespace $(SUBSTRATE_ATESPACE)"; \
		echo "  kubectl ate create actor $(SUBSTRATE_ACTOR) -a $(SUBSTRATE_ATESPACE) --template=$(SUBSTRATE_TEMPLATE)"; \
	fi

# Export the orchestrator image to a portable archive (docs/adr/0023).
image-orchestrator-save: image-orchestrator
	$(CONTAINER_ENGINE) save $(ORCHESTRATOR_IMAGE) -o zumble-zay-orchestrator-image.tar

# Load the orchestrator control-plane image into kind (same engine-agnostic
# archive path as kind-load).
kind-load-orchestrator: image-orchestrator-save
	kind load image-archive zumble-zay-orchestrator-image.tar --name $(KIND_CLUSTER)
	rm -f zumble-zay-orchestrator-image.tar

# Create the kind cluster if it does not already exist. For LAUNCHER=substrate the
# cluster must be feature-gated at creation AND carry the full ate-system (see
# substrate-cluster), so cluster-up resolves to that heavy bring-up; every other
# launcher gets a plain kind cluster.
ifeq ($(LAUNCHER),substrate)
cluster-up: substrate-cluster
else
cluster-up:
	@command -v kind >/dev/null || { echo "kind not installed (https://kind.sigs.k8s.io)"; exit 1; }
	@kind get clusters 2>/dev/null | grep -qx $(KIND_CLUSTER) || kind create cluster --name $(KIND_CLUSTER)
endif

# Delete the kind cluster.
cluster-down:
	-kind delete cluster --name $(KIND_CLUSTER)

# EXPERIMENTAL (docs/adr/0035): stand up a feature-gated kind cluster + the full
# ate-system so LAUNCHER=substrate has a substrate to run on. Agent Substrate needs
# CREATE-TIME apiserver feature gates (ClusterTrustBundle, ClusterTrustBundleProjection,
# PodCertificateRequest + the certificates.k8s.io/v1beta1 runtimeConfig) that cannot
# be added to a running cluster, and its control-plane images are ko-built from source
# (no published images), so bootstrapping = cloning Substrate and driving ITS kind
# scripts. cluster-up delegates here for LAUNCHER=substrate, so this runs as part of
# `LAUNCHER=substrate make dev-up`. It DELETES and recreates the '$(KIND_CLUSTER)'
# cluster and requires docker (Substrate's scripts assume docker), go, and git.
# Pinned to SUBSTRATE_REF (a v0.0.0 moving target).
substrate-cluster:
	@command -v docker >/dev/null || { echo "docker required (Agent Substrate's kind scripts assume docker, not podman)"; exit 1; }
	@command -v go >/dev/null || { echo "go required (ate-system is ko-built from source)"; exit 1; }
	@command -v git >/dev/null || { echo "git required"; exit 1; }
	@echo "EXPERIMENTAL: recreating the '$(KIND_CLUSTER)' kind cluster with the Agent Substrate feature"
	@echo "             gates + the full ate-system (gVisor, rustfs, valkey, atenet, atelet)."
	@echo "             This DELETES the existing '$(KIND_CLUSTER)' cluster (feature gates are create-time)."
	rm -rf $(SUBSTRATE_CLONE_DIR)
	git clone --depth 1 --branch $(SUBSTRATE_REF) https://github.com/agent-substrate/substrate $(SUBSTRATE_CLONE_DIR)
	cd $(SUBSTRATE_CLONE_DIR) && KIND_CLUSTER_NAME=$(KIND_CLUSTER) ./hack/create-kind-cluster.sh
	cd $(SUBSTRATE_CLONE_DIR) && KIND_CLUSTER_NAME=$(KIND_CLUSTER) KUBECTL_CONTEXT=kind-$(KIND_CLUSTER) ./hack/install-ate-kind.sh --deploy-ate-system
	# Install the kubectl-ate plugin from the same checkout so substrate-install can
	# create the atespace + actor. Without it the WorkerPool/ActorTemplate still apply,
	# but no actor exists for the router to route to and every ZZ job fails to dispatch.
	# Lands in $$(go env GOPATH)/bin (must be on PATH for the `command -v kubectl-ate` check).
	@echo "installing the kubectl-ate plugin (go install ./cmd/kubectl-ate)"
	cd $(SUBSTRATE_CLONE_DIR) && go install ./cmd/kubectl-ate

# One shot: build the images from the current source, stand up a kind cluster,
# load them, deploy the dev overlay, and wait until both tiers are ready. Three
# images are loaded: the web tier, the orchestrator control plane (docs/adr/0023),
# and the runtime. LAUNCHER selects the agent substrate (default k8s-job); set
# LAUNCHER=agent-sandbox to build the orchestrator with the agent_sandbox tag,
# install the agent-sandbox controller + CRDs, and deploy with it selected
# (docs/adr/0026).
dev-up: cluster-up kind-load kind-load-orchestrator kind-load-runtime
	@kubectl create namespace $(KUBE_NS) --dry-run=client -o yaml | kubectl apply -f -
	@kubectl -n $(KUBE_NS) get secret zumble-zay-secrets >/dev/null 2>&1 || \
		kubectl -n $(KUBE_NS) create secret generic zumble-zay-secrets \
			--from-literal=SESSION_SECRET="$$(openssl rand -base64 48)"
	# The dev overlay always runs the agentgateway as the agents' LLM egress proxy,
	# and it resolves the provider key from zumble-zay-secrets/AI_TOKEN at startup.
	# Unlike SESSION_SECRET, this key cannot be generated — it is
	# a real provider credential. A missing key hard-fails the gateway pod
	# (CreateContainerConfigError), which drops every runtime to the stub ranker.
	# Seed it from $$AI_TOKEN when exported (idempotent — patched in even on a
	# pre-existing secret), and warn clearly when it is absent so the failure is not
	# a mystery on a fresh cluster.
	@if [ -n "$$AI_TOKEN" ]; then \
		kubectl -n $(KUBE_NS) patch secret zumble-zay-secrets --type merge \
			-p "{\"stringData\":{\"AI_TOKEN\":\"$$AI_TOKEN\"}}" >/dev/null && \
		echo "seeded AI_TOKEN into zumble-zay-secrets (agentgateway provider key)"; \
	elif ! kubectl -n $(KUBE_NS) get secret zumble-zay-secrets -o jsonpath='{.data.AI_TOKEN}' 2>/dev/null | grep -q .; then \
		echo "WARNING: zumble-zay-secrets has no AI_TOKEN and \$$AI_TOKEN is unset;"; \
		echo "         the agentgateway pod will stay in CreateContainerConfigError and"; \
		echo "         runtimes will fall back to the stub ranker. Enable model ranking with:"; \
		echo "           export AI_TOKEN=<provider key> && make dev-up"; \
	fi
	# OAuth provider credentials are external app registrations (a GitHub OAuth App,
	# etc.), so like AI_TOKEN dev-up cannot generate them -- it can only relay what
	# you already have. Seed each provider client ID/secret from the environment when
	# exported, so exporting GITHUB_CLIENT_ID + GITHUB_CLIENT_SECRET before a fresh
	# dev-up wires login with no manual patch. Unset keys are skipped (that provider
	# stays disabled); the merge patch leaves every other secret key untouched.
	@for k in GITHUB_CLIENT_ID GITHUB_CLIENT_SECRET GOOGLE_CLIENT_ID GOOGLE_CLIENT_SECRET MICROSOFT_CLIENT_ID MICROSOFT_CLIENT_SECRET; do \
		v=$$(printenv "$$k" || true); \
		if [ -n "$$v" ]; then \
			kubectl -n $(KUBE_NS) patch secret zumble-zay-secrets --type merge \
				-p "{\"stringData\":{\"$$k\":\"$$v\"}}" >/dev/null && \
			echo "seeded $$k into zumble-zay-secrets"; \
		fi; \
	done
	# A missing GitHub client ID just means login is off (an intended mode), so this
	# is a gentle note, not a failure -- it is the exact "No sign-in providers are
	# configured" state the landing page shows.
	@kubectl -n $(KUBE_NS) get secret zumble-zay-secrets -o jsonpath='{.data.GITHUB_CLIENT_ID}' 2>/dev/null | grep -q . || \
		echo "note: GITHUB_CLIENT_ID unset -- sign-in stays disabled; export GITHUB_CLIENT_ID/GITHUB_CLIENT_SECRET and re-run dev-up to enable login"
	# Optional agent-sandbox substrate: install its controller + CRDs so the
	# orchestrator can create Sandboxes (docs/adr/0026). Only when selected; the
	# CRD wait is best-effort so a slow controller rollout does not fail dev-up.
	@if [ "$(LAUNCHER)" = "agent-sandbox" ]; then \
		echo "installing agent-sandbox $(AGENT_SANDBOX_VERSION) (controller + CRDs)"; \
		kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/$(AGENT_SANDBOX_VERSION)/manifest.yaml; \
		kubectl wait --for=condition=Established crd/sandboxes.agents.x-k8s.io --timeout=60s || true; \
	fi
	# Optional OpenSandbox substrate (docs/adr/0027): load the shell-bearing runtime
	# image and install the OpenSandbox platform so the orchestrator has a control
	# plane to drive. Only when selected. EXPERIMENTAL — see opensandbox-install.
	@if [ "$(LAUNCHER)" = "opensandbox" ]; then \
		echo "loading shell runtime image + installing OpenSandbox (experimental)"; \
		$(MAKE) kind-load-runtime-shell opensandbox-install; \
	fi
	# Optional kagent substrate (docs/adr/0024): load the A2A runtime-server image
	# and install the kagent control plane so the orchestrator can dispatch jobs to
	# a durable agent. Only when selected. The zz-runtime BYO Agent is applied after
	# the overlay (below), since it now lives in the zumble-zay namespace. The
	# orchestrator's LAUNCHER + KAGENT_* defaults need no extra env, so the generic
	# LAUNCHER override below is all the orchestrator wiring required.
	@if [ "$(LAUNCHER)" = "kagent" ]; then \
		echo "loading A2A runtime image + installing the kagent control plane"; \
		$(MAKE) kind-load-runtime-a2a kagent-install; \
	fi
	# Optional Agent Substrate substrate (docs/adr/0035): the cluster + ate-system
	# were bootstrapped by cluster-up (substrate-cluster). Now make ZZ's actor real on
	# it — push the runtime-a2a image to the ate-system registry as a DIGEST (the
	# ActorTemplate rejects unpinned images), resolve the gVisor ateom herder via
	# Substrate's ko, and apply the WorkerPool/ActorTemplate + atespace/actor against
	# the in-cluster rustfs snapshot bucket. Only when selected. EXPERIMENTAL.
	@if [ "$(LAUNCHER)" = "substrate" ]; then \
		echo "pushing the runtime-a2a actor image to the ate-system registry ($(SUBSTRATE_REGISTRY))"; \
		docker build -f Dockerfile.runtime-a2a -t $(SUBSTRATE_REGISTRY)/zumble-zay-runtime-a2a:dev . && \
		docker push $(SUBSTRATE_REGISTRY)/zumble-zay-runtime-a2a:dev && \
		actor_img=$$(docker inspect --format '{{index .RepoDigests 0}}' $(SUBSTRATE_REGISTRY)/zumble-zay-runtime-a2a:dev) && \
		echo "resolving the gVisor ateom herder image via Substrate's ko" && \
		ateom_img=$$(cd $(SUBSTRATE_CLONE_DIR) && KO_DOCKER_REPO=$(SUBSTRATE_REGISTRY) KO_DEFAULTPLATFORMS=linux/$$(go env GOARCH) ./hack/run-tool.sh ko build --base-import-paths --push ./cmd/ateom-gvisor) && \
		$(MAKE) substrate-install SUBSTRATE_RUNTIME_IMAGE=$$actor_img SUBSTRATE_ATEOM_IMAGE=$$ateom_img SUBSTRATE_SNAPSHOT_LOCATION=gs://ate-snapshots/zz-runtime/; \
	fi
	# Enforce the base NetworkPolicy in dev (docs/adr/0033): the orchestrator ships
	# a default-deny control-API policy, but kindnet ignores NetworkPolicy, so
	# install the kube-network-policies controller to make it live. Best-effort —
	# the policy is defense-in-depth over the auth controls (docs/adr/0031, 0032)
	# and fail-open, so a transient install failure degrades to the pre-0033 kindnet
	# behavior rather than blocking the dev loop. Skip with an empty version; also
	# skipped for LAUNCHER=substrate, whose ate-system runs its own cluster networking
	# (atenet: Envoy + nftables), which a second nftables controller can fight.
	@if [ "$(LAUNCHER)" = "substrate" ]; then \
		echo "skipping kube-network-policies on the substrate cluster (ate's atenet owns cluster networking; a second nftables controller can conflict)"; \
	elif [ -n "$(KUBE_NETWORK_POLICIES_VERSION)" ]; then \
		echo "installing kube-network-policies $(KUBE_NETWORK_POLICIES_VERSION) (enforces NetworkPolicy on kindnet)"; \
		kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/kube-network-policies/$(KUBE_NETWORK_POLICIES_VERSION)/install.yaml \
			|| echo "WARNING: kube-network-policies install failed; the orchestrator NetworkPolicy stays unenforced in dev (defense-in-depth only, not a blocker)"; \
		kubectl -n kube-system rollout status ds/kube-network-policies --timeout=90s || true; \
	fi
	kubectl apply -k deploy/k8s/overlays/dev
	# The kagent BYO Agent lives in the zumble-zay namespace (the kagent controller
	# reconciles cluster-wide), so it is applied here — after the overlay creates
	# that namespace, not in kagent-install which runs before it exists.
	@if [ "$(LAUNCHER)" = "kagent" ]; then \
		echo "applying the zz-runtime BYO Agent in the $(KUBE_NS) namespace"; \
		kubectl apply -f deploy/k8s/kagent/zz-runtime-agent.yaml; \
	fi
	# Select the launcher on the orchestrator when it differs from the ConfigMap
	# default (k8s-job): an explicit container env wins over envFrom, so this
	# overrides the deployed LAUNCHER without editing the kustomize ConfigMap.
	@if [ "$(LAUNCHER)" != "k8s-job" ]; then \
		echo "selecting LAUNCHER=$(LAUNCHER) on the orchestrator"; \
		kubectl -n $(KUBE_NS) set env deploy/zumble-zay-orchestrator LAUNCHER=$(LAUNCHER); \
	fi
	# OpenSandbox needs more than the launcher name: the endpoint + API key to reach
	# the OpenSandbox server, the shell-bearing runtime image, and a cross-namespace
	# ZZ base URL (sandboxes run in the OpenSandbox namespace, not the web tier's).
	# The model token is NOT wired here — OpenSandbox cannot use a Secret reference,
	# so set AI_TOKEN on the orchestrator yourself to enable LLM ranking; without it
	# the runtime falls back to the stub ranker (docs/adr/0027).
	@if [ "$(LAUNCHER)" = "opensandbox" ]; then \
		kubectl -n $(KUBE_NS) set env deploy/zumble-zay-orchestrator \
			OPENSANDBOX_ENDPOINT=http://opensandbox-server.$(OPENSANDBOX_NAMESPACE).svc:80/v1 \
			OPENSANDBOX_API_KEY=$(OPENSANDBOX_API_KEY) \
			OPENSANDBOX_RUNTIME_IMAGE=$(RUNTIME_SHELL_IMAGE) \
			RUNTIME_ZZ_BASE_URL=http://zumble-zay.$(KUBE_NS).svc.cluster.local:8080; \
	fi
	# Agent Substrate needs the router endpoint + actor coordinates so the launcher
	# can address the durable actor through the atenet-router (docs/adr/0035).
	@if [ "$(LAUNCHER)" = "substrate" ]; then \
		kubectl -n $(KUBE_NS) set env deploy/zumble-zay-orchestrator \
			SUBSTRATE_ROUTER_URL=$(SUBSTRATE_ROUTER_URL) \
			SUBSTRATE_ATESPACE=$(SUBSTRATE_ATESPACE) \
			SUBSTRATE_ACTOR=$(SUBSTRATE_ACTOR); \
	fi
	# The image tag (:dev) is mutable, so `apply` is a no-op when only the image
	# content changed — the Deployment spec is identical and no new pod is
	# created, leaving the old code running. kind-load already replaced the image
	# on the node, so force a rollout to adopt it. (Agent Jobs need no equivalent:
	# each is a fresh pod that pulls IfNotPresent from the reloaded node image.)
	kubectl -n $(KUBE_NS) rollout restart deploy/zumble-zay deploy/zumble-zay-orchestrator
	kubectl -n $(KUBE_NS) rollout status deploy/zumble-zay --timeout=120s
	kubectl -n $(KUBE_NS) rollout status deploy/zumble-zay-orchestrator --timeout=120s
	# On a re-run the zz-runtime agent image (:dev) is mutable, so kagent's
	# Deployment keeps the old code until restarted — mirror the web/orchestrator
	# rollout for the durable agent. Best-effort: on the very first install the
	# Deployment may not exist yet (the kagent controller reconciles it
	# asynchronously), and the next dev-up adopts the reloaded image.
	@if [ "$(LAUNCHER)" = "kagent" ]; then \
		echo "restarting the zz-runtime agent to adopt the reloaded image"; \
		kubectl -n $(KUBE_NS) rollout restart deploy/zz-runtime 2>/dev/null || true; \
	fi
	@echo
	@echo "zumble-zay is running. Expose it with:  make dev-forward"
	@echo "then:  curl localhost:8080/healthz"

# Tear down the whole dev environment.
dev-down: cluster-down

# Port-forward the service to localhost:8080 (blocks).
dev-forward:
	kubectl -n $(KUBE_NS) port-forward deploy/zumble-zay 8080:8080

# Tail the web tier logs.
dev-logs:
	kubectl -n $(KUBE_NS) logs -f deploy/zumble-zay

# Tail the orchestrator control-plane logs (docs/adr/0023).
dev-logs-orchestrator:
	kubectl -n $(KUBE_NS) logs -f deploy/zumble-zay-orchestrator
