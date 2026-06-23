.PHONY: build run test tidy vet fmt clean image image-save kind-load engine \
        cluster-up cluster-down dev-up dev-down dev-forward dev-logs

BIN := bin/server

# Container engine autodetection: prefer podman (common on macOS), fall back to
# docker. Override explicitly with `make image CONTAINER_ENGINE=docker`.
CONTAINER_ENGINE ?= $(shell command -v podman 2>/dev/null || command -v docker 2>/dev/null)
# localhost-qualified so the name is identical under podman and docker (podman
# auto-prefixes unqualified names with localhost/). Kubernetes treats the
# localhost registry as local, so kind-loaded images resolve without a pull.
IMAGE ?= localhost/zumble-zay:dev

KIND_CLUSTER ?= zumble-zay
KUBE_NS := zumble-zay

# kind must use the podman provider when podman is the chosen engine.
ifeq ($(findstring podman,$(CONTAINER_ENGINE)),podman)
export KIND_EXPERIMENTAL_PROVIDER = podman
endif

build:
	go build -o $(BIN) ./cmd/server

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
	rm -rf bin zumble-zay-image.tar

# Print the detected container engine.
engine:
	@test -n "$(CONTAINER_ENGINE)" || { echo "no container engine found (install podman or docker)"; exit 1; }
	@echo "using container engine: $(CONTAINER_ENGINE)"

# Build the image with whichever engine is available.
image: engine
	$(CONTAINER_ENGINE) build -t $(IMAGE) .

# Export the image to a portable archive (works with docker and podman).
image-save: image
	$(CONTAINER_ENGINE) save $(IMAGE) -o zumble-zay-image.tar

# Load the image into a local kind cluster via the engine-agnostic archive path
# (kind's `load docker-image` is docker-only; `load image-archive` is not).
kind-load: image-save
	kind load image-archive zumble-zay-image.tar --name $(KIND_CLUSTER)
	rm -f zumble-zay-image.tar

# Create the kind cluster if it does not already exist.
cluster-up:
	@command -v kind >/dev/null || { echo "kind not installed (https://kind.sigs.k8s.io)"; exit 1; }
	@kind get clusters 2>/dev/null | grep -qx $(KIND_CLUSTER) || kind create cluster --name $(KIND_CLUSTER)

# Delete the kind cluster.
cluster-down:
	-kind delete cluster --name $(KIND_CLUSTER)

# One shot: build the image from the current source, stand up a kind cluster,
# load the image, deploy the dev overlay, and wait until the app is ready.
dev-up: cluster-up kind-load
	@kubectl create namespace $(KUBE_NS) --dry-run=client -o yaml | kubectl apply -f -
	@kubectl -n $(KUBE_NS) get secret zumble-zay-secrets >/dev/null 2>&1 || \
		kubectl -n $(KUBE_NS) create secret generic zumble-zay-secrets \
			--from-literal=SESSION_SECRET="$$(openssl rand -base64 48)"
	kubectl apply -k deploy/k8s/overlays/dev
	kubectl -n $(KUBE_NS) rollout status deploy/zumble-zay --timeout=120s
	@echo
	@echo "zumble-zay is running. Expose it with:  make dev-forward"
	@echo "then:  curl localhost:8080/healthz"

# Tear down the whole dev environment.
dev-down: cluster-down

# Port-forward the service to localhost:8080 (blocks).
dev-forward:
	kubectl -n $(KUBE_NS) port-forward deploy/zumble-zay 8080:8080

# Tail the application logs.
dev-logs:
	kubectl -n $(KUBE_NS) logs -f deploy/zumble-zay
