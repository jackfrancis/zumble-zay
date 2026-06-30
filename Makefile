.PHONY: build run test tidy vet fmt clean image image-save kind-load engine \
        cluster-up cluster-down dev-up dev-down dev-forward dev-logs dev-logs-orchestrator \
        build-runtime image-runtime image-runtime-save kind-load-runtime \
        build-orchestrator image-orchestrator image-orchestrator-save kind-load-orchestrator vendor-primer

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
# agent-sandbox is the only build-tagged substrate today; compile it into the
# orchestrator image only when it is the selected launcher.
ORCHESTRATOR_GO_TAGS := $(if $(filter agent-sandbox,$(LAUNCHER)),agent_sandbox,)
# Pinned agent-sandbox release for the optional controller + CRDs install.
AGENT_SANDBOX_VERSION ?= v0.5.0

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
dev-up: cluster-up kind-load kind-load-orchestrator kind-load-runtime
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
	# Optional agent-sandbox substrate: install its controller + CRDs so the
	# orchestrator can create Sandboxes (docs/adr/0026). Only when selected; the
	# CRD wait is best-effort so a slow controller rollout does not fail dev-up.
	@if [ "$(LAUNCHER)" = "agent-sandbox" ]; then \
		echo "installing agent-sandbox $(AGENT_SANDBOX_VERSION) (controller + CRDs)"; \
		kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/$(AGENT_SANDBOX_VERSION)/manifest.yaml; \
		kubectl wait --for=condition=Established crd/sandboxes.agents.x-k8s.io --timeout=60s || true; \
	fi
	kubectl apply -k deploy/k8s/overlays/dev
	# Select the launcher on the orchestrator when it differs from the ConfigMap
	# default (k8s-job): an explicit container env wins over envFrom, so this
	# overrides the deployed LAUNCHER without editing the kustomize ConfigMap.
	@if [ "$(LAUNCHER)" != "k8s-job" ]; then \
		echo "selecting LAUNCHER=$(LAUNCHER) on the orchestrator"; \
		kubectl -n $(KUBE_NS) set env deploy/zumble-zay-orchestrator LAUNCHER=$(LAUNCHER); \
	fi
	# The image tag (:dev) is mutable, so `apply` is a no-op when only the image
	# content changed — the Deployment spec is identical and no new pod is
	# created, leaving the old code running. kind-load already replaced the image
	# on the node, so force a rollout to adopt it. (Agent Jobs need no equivalent:
	# each is a fresh pod that pulls IfNotPresent from the reloaded node image.)
	kubectl -n $(KUBE_NS) rollout restart deploy/zumble-zay deploy/zumble-zay-orchestrator
	kubectl -n $(KUBE_NS) rollout status deploy/zumble-zay --timeout=120s
	kubectl -n $(KUBE_NS) rollout status deploy/zumble-zay-orchestrator --timeout=120s
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
