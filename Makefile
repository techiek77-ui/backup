IMAGE    ?= podspawner:dev
PLATFORM ?= linux/amd64
CLUSTER  ?= podspawner

.PHONY: build test vet docker-build kind-up kind-down demo oke-push oke-deploy manifests

build:
	CGO_ENABLED=0 go build -o bin/controller ./cmd/controller

vet:
	go vet ./...

test: vet
	go test -race ./...

## Container image (buildx so PLATFORM can target OKE amd64 or arm64 pools)
docker-build:
	docker buildx build --platform $(PLATFORM) -t $(IMAGE) --load .

## Local kind demo
kind-up:
	CLUSTER=$(CLUSTER) ./hack/kind-up.sh

demo:
	./hack/demo.sh

kind-down:
	kind delete cluster --name $(CLUSTER)

## OKE: build for the node pool arch, push to OCIR, apply the overlay with
## the image substituted. Example:
##   make oke-push oke-deploy IMAGE=fra.ocir.io/axabcdefghij/podspawner:0.1.0
##   make oke-deploy OVERLAY=oke-managed-nodes IMAGE=...   (controller-managed nodes)
oke-push:
	docker buildx build --platform $(PLATFORM) -t $(IMAGE) --push .

OVERLAY ?= oke

oke-deploy:
	kubectl kustomize deploy/overlays/$(OVERLAY) \
	  | sed 's#REGION_KEY.ocir.io/TENANCY_NAMESPACE/podspawner:0.1.0#$(IMAGE)#g' \
	  | kubectl apply -f -
	kubectl -n podspawner rollout status deploy/podspawner --timeout=180s

manifests:
	@kubectl kustomize deploy/overlays/kind
