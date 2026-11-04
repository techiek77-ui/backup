#!/usr/bin/env bash
# Create a local kind cluster, label one node, build + load the image and
# deploy the controller. Idempotent: safe to re-run after code changes.
set -euo pipefail

CLUSTER="${CLUSTER:-podspawner}"
IMAGE="${IMAGE:-podspawner:dev}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

for tool in kind kubectl docker; do
  command -v "$tool" >/dev/null || { echo "missing required tool: $tool" >&2; exit 1; }
done

if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  echo "==> Creating kind cluster '$CLUSTER'"
  kind create cluster --name "$CLUSTER" --config "$ROOT/hack/kind-config.yaml" --wait 120s
else
  echo "==> Reusing kind cluster '$CLUSTER'"
fi
kubectl config use-context "kind-$CLUSTER" >/dev/null

echo "==> Labelling node ${CLUSTER}-worker with workload=worker (worker2 stays unlabelled)"
kubectl label node "${CLUSTER}-worker" workload=worker --overwrite
kubectl label node "${CLUSTER}-worker2" workload- >/dev/null 2>&1 || true

echo "==> Building $IMAGE"
docker build -t "$IMAGE" "$ROOT"

echo "==> Loading image into kind"
kind load docker-image "$IMAGE" --name "$CLUSTER"

echo "==> Deploying"
kubectl apply -k "$ROOT/deploy/overlays/kind"
kubectl -n podspawner rollout restart deploy/podspawner >/dev/null 2>&1 || true
kubectl -n podspawner rollout status deploy/podspawner --timeout=120s

echo
echo "Controller is up. Run ./hack/demo.sh to submit work."
