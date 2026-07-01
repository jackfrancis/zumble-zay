.PHONY: build run test tidy vet fmt clean image image-save kind-load engine \
        cluster-up cluster-down dev-up dev-down dev-forward dev-logs dev-logs-orchestrator \
        build-runtime image-runtime image-runtime-save kind-load-runtime \
        build-orchestrator image-orchestrator image-orchestrator-save kind-load-orchestrator vendor-primer \
        image-runtime-shell image-runtime-shell-save kind-load-runtime-shell opensandbox-install \
        image-runtime-a2a image-runtime-a2a-save kind-load-runtime-a2a kagent-install \
        image-ray image-ray-save kind-load-ray

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
ORCHESTRATOR_GO_TAGS_LIST += $(if $(filter ray,$(LAUNCHER)),ray,)
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

# Optional ray/kuberay substrate (LAUNCHER=ray, docs/adr/0028). When selected,
# `make dev-up` installs the KubeRay operator at this version, builds+loads the
# Ray image (which bundles /runtime), and applies a standing RayCluster. The
# image/cluster names must match deploy/ray/raycluster.yaml and RAY_CLUSTER.
KUBERAY_VERSION  ?= 1.1.1
RAY_IMAGE        ?= localhost/zz-ray:dev
RAY_CLUSTER_NAME ?= zz-ray
# When LAUNCHER=ray, dev-up additionally builds+loads the Ray image; empty
# otherwise, so a default dev-up is unchanged.
DEV_UP_RAY_PREREQ = $(if $(filter ray,$(LAUNCHER)),kind-load-ray,)

# Vendored GitHub Primer CSS (the UI's design system, served from the binary).
# `make vendor-primer` refreshes these at the pinned versions; bump the versions
# and re-run to scoop in updates.
PRIMER_CSS_VERSION ?= 22.3.0
PRIMER_PRIMITIVES_VERSION ?= 11.9.0
PRIMER_DIR := internal/webui/static/primer

KIND_CLUSTER ?= zumble-zay
KUBE_NS := zumble-zay

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
# agentgateway, not kagent's ModelConfig), then the zz-runtime BYO Agent, which
# kagent reconciles into the durable Deployment the orchestrator dispatches to.
# The Agent's image must be kind-loaded first (dev-up does this).
kagent-install:
	@command -v helm >/dev/null || { echo "helm not installed (https://helm.sh)"; exit 1; }
	helm upgrade --install kagent-crds oci://ghcr.io/kagent-dev/kagent/helm/kagent-crds \
		--version $(KAGENT_VERSION) --namespace $(KAGENT_NAMESPACE) --create-namespace --wait --timeout 300s
	helm upgrade --install kagent oci://ghcr.io/kagent-dev/kagent/helm/kagent \
		--version $(KAGENT_VERSION) --namespace $(KAGENT_NAMESPACE) \
		--set providers.default=openAI --set providers.openAI.apiKey=sk-kagent-dev-unused \
		--wait --timeout 360s
	kubectl apply -f deploy/k8s/kagent/zz-runtime-agent.yaml

# Build the Ray image that bundles the /runtime binary onto a Ray base
# (docs/adr/0028). A RayJob runs entrypoint=/runtime on the cluster, so the
# RayCluster image must carry the runtime binary. Engine-agnostic (podman/docker)
# via the same plain-build + save + load-archive path as the other images.
image-ray: engine
	$(CONTAINER_ENGINE) build -f deploy/ray/Dockerfile.ray -t $(RAY_IMAGE) .

# Export the Ray image to a portable archive.
image-ray-save: image-ray
	$(CONTAINER_ENGINE) save $(RAY_IMAGE) -o zumble-zay-ray-image.tar

# Load the Ray image into kind so the RayCluster resolves it without a registry
# pull (same engine-agnostic archive path as kind-load). Run before LAUNCHER=ray.
kind-load-ray: image-ray-save
	kind load image-archive zumble-zay-ray-image.tar --name $(KIND_CLUSTER)
	rm -f zumble-zay-ray-image.tar

# Export the orchestrator image to a portable archive (docs/adr/0023).
image-orchestrator-save: image-orchestrator
	$(CONTAINER_ENGINE) save $(ORCHESTRATOR_IMAGE) -o zumble-zay-orchestrator-image.tar

# Load the orchestrator control-plane image into kind (same engine-agnostic
# archive path as kind-load).
kind-load-orchestrator: image-orchestrator-save
	kind load image-archive zumble-zay-orchestrator-image.tar --name $(KIND_CLUSTER)
	rm -f zumble-zay-orchestrator-image.tar

# Create the kind cluster if it does not already exist.
cluster-up:
	@command -v kind >/dev/null || { echo "kind not installed (https://kind.sigs.k8s.io)"; exit 1; }
	@kind get clusters 2>/dev/null | grep -qx $(KIND_CLUSTER) || kind create cluster --name $(KIND_CLUSTER)

# Delete the kind cluster.
cluster-down:
	-kind delete cluster --name $(KIND_CLUSTER)

# One shot: build the images from the current source, stand up a kind cluster,
# load them, deploy the dev overlay, and wait until both tiers are ready. Three
# images are loaded: the web tier, the orchestrator control plane (docs/adr/0023),
# and the runtime. LAUNCHER selects the agent substrate (default k8s-job); set
# LAUNCHER=agent-sandbox to build the orchestrator with the agent_sandbox tag,
# install the agent-sandbox controller + CRDs, and deploy with it selected
# (docs/adr/0026).
dev-up: cluster-up kind-load kind-load-orchestrator kind-load-runtime $(DEV_UP_RAY_PREREQ)
	@kubectl create namespace $(KUBE_NS) --dry-run=client -o yaml | kubectl apply -f -
	@kubectl -n $(KUBE_NS) get secret zumble-zay-secrets >/dev/null 2>&1 || \
		kubectl -n $(KUBE_NS) create secret generic zumble-zay-secrets \
			--from-literal=SESSION_SECRET="$$(openssl rand -base64 48)" \
			--from-literal=CONTROL_PLANE_TOKEN="$$(openssl rand -base64 48)"
	# Ensure the control-plane token exists even on a secret created before it was
	# introduced: the create above is skipped when the secret already exists, so a
	# pre-existing secret would otherwise lack the key and crash-loop the orchestrator.
	@kubectl -n $(KUBE_NS) get secret zumble-zay-secrets -o jsonpath='{.data.CONTROL_PLANE_TOKEN}' 2>/dev/null | grep -q . || \
		kubectl -n $(KUBE_NS) patch secret zumble-zay-secrets --type merge \
			-p "{\"stringData\":{\"CONTROL_PLANE_TOKEN\":\"$$(openssl rand -base64 48 | tr -d '\n')\"}}"
	# The dev overlay always runs the agentgateway as the agents' LLM egress proxy,
	# and it resolves the provider key from zumble-zay-secrets/AI_TOKEN at startup.
	# Unlike SESSION_SECRET/CONTROL_PLANE_TOKEN, this key cannot be generated — it is
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
	# and install kagent (control plane + the zz-runtime BYO Agent) so the
	# orchestrator can dispatch jobs to a durable agent. Only when selected. The
	# orchestrator's LAUNCHER + KAGENT_* defaults (kagent-controller.kagent, the
	# zz-runtime agent) need no extra env, so the generic LAUNCHER override below is
	# all the orchestrator wiring required.
	@if [ "$(LAUNCHER)" = "kagent" ]; then \
		echo "loading A2A runtime image + installing kagent"; \
		$(MAKE) kind-load-runtime-a2a kagent-install; \
	fi
	# Optional ray/kuberay substrate: install the KubeRay operator (+ CRDs) and a
	# standing RayCluster so the orchestrator can create RayJobs (docs/adr/0028).
	# Only when selected. helm upgrade --install keeps it idempotent; the readiness
	# waits are best-effort so a slow rollout does not fail dev-up.
	@if [ "$(LAUNCHER)" = "ray" ]; then \
		command -v helm >/dev/null || { echo "helm not installed (https://helm.sh)"; exit 1; }; \
		echo "installing KubeRay operator $(KUBERAY_VERSION)"; \
		helm repo add kuberay https://ray-project.github.io/kuberay-helm/ >/dev/null 2>&1 || true; \
		helm repo update kuberay >/dev/null 2>&1 || true; \
		helm upgrade --install kuberay-operator kuberay/kuberay-operator \
			--version $(KUBERAY_VERSION) -n kuberay-system --create-namespace; \
		kubectl -n kuberay-system rollout status deploy/kuberay-operator --timeout=120s || true; \
		echo "applying RayCluster $(RAY_CLUSTER_NAME)"; \
		kubectl apply -f deploy/ray/raycluster.yaml; \
		kubectl -n $(KUBE_NS) wait --for=condition=Ready pod -l ray.io/node-type=head --timeout=180s || true; \
	fi
	kubectl apply -k deploy/k8s/overlays/dev
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
	# The ray launcher needs the standing cluster's name/namespace (docs/adr/0028).
	@if [ "$(LAUNCHER)" = "ray" ]; then \
		kubectl -n $(KUBE_NS) set env deploy/zumble-zay-orchestrator \
			RAY_CLUSTER=$(RAY_CLUSTER_NAME) RAY_NAMESPACE=$(KUBE_NS); \
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
		kubectl -n $(KAGENT_NAMESPACE) rollout restart deploy/zz-runtime 2>/dev/null || true; \
	fi
	@echo
	@echo "zumble-zay is running. Expose it with:  make dev-forward"
	@echo "then:  curl localhost:8080/healthz"
	@if [ "$(LAUNCHER)" = "ray" ]; then \
		echo "ray substrate ready: RayCluster $(RAY_CLUSTER_NAME); jobs run as RayJobs (kubectl -n $(KUBE_NS) get rayjobs)"; \
		echo "set the model token:  kubectl -n $(KUBE_NS) patch secret zumble-zay-secrets --type merge -p '{\"stringData\":{\"AI_TOKEN\":\"<copilot-token>\"}}'"; \
	fi

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
