#!/usr/bin/env bash
# Pre-flight for NODE_MODE=oke: checks every node pool referenced by
# SIZE_CLASSES with the OCI CLI and kubectl before the controller is deployed.
#
#   SIZE_CLASSES='{"small":{"cpu":"1","memory":"2Gi","nodePoolId":"ocid1.nodepool..."}}' ./hack/oke-preflight.sh
#
# Needs: oci (configured), kubectl (pointing at the cluster), jq.
set -euo pipefail

for tool in oci kubectl jq; do
  command -v "$tool" >/dev/null || { echo "missing required tool: $tool" >&2; exit 1; }
done
: "${SIZE_CLASSES:?set SIZE_CLASSES to the JSON you will give the controller}"
LABEL_KEY="${LABEL_KEY:-workload}"
LABEL_VALUE="${LABEL_VALUE:-worker}"
rc=0
warn() { echo "  WARN  $*"; rc=1; }
ok()   { echo "  ok    $*"; }

echo "==> kubectl context: $(kubectl config current-context)"
if kubectl auth can-i list nodes >/dev/null 2>&1; then ok "can list nodes"; else warn "cannot list nodes with this kubeconfig"; fi
echo "    nodes currently labelled ${LABEL_KEY}=${LABEL_VALUE}: $(kubectl get nodes -l "${LABEL_KEY}=${LABEL_VALUE}" --no-headers 2>/dev/null | wc -l | tr -d ' ')"

for class in $(jq -r 'keys[]' <<<"$SIZE_CLASSES"); do
  pool=$(jq -r --arg c "$class" '.[$c].nodePoolId // empty' <<<"$SIZE_CLASSES")
  echo
  echo "==> class '$class'"
  if [ -z "$pool" ]; then
    echo "  info  no nodePoolId: scheduled normally (autoscaler / virtual nodes), not controller-managed"
    continue
  fi
  if ! data=$(oci ce node-pool get --node-pool-id "$pool" 2>&1); then
    warn "oci ce node-pool get failed for $pool: $(echo "$data" | head -3 | tr '\n' ' ')"
    continue
  fi
  name=$(jq -r '.data.name' <<<"$data")
  state=$(jq -r '.data["lifecycle-state"]' <<<"$data")
  size=$(jq -r '.data["node-config-details"].size // "null"' <<<"$data")
  shape=$(jq -r '.data["node-shape"]' <<<"$data")
  ocpus=$(jq -r '.data["node-shape-config"].ocpus // "-"' <<<"$data")
  mem=$(jq -r '.data["node-shape-config"]["memory-in-gbs"] // "-"' <<<"$data")
  k8s=$(jq -r '.data["kubernetes-version"]' <<<"$data")
  nodes=$(jq -r '[.data.nodes[]? | select(.["lifecycle-state"] != "DELETED")] | length' <<<"$data")
  label=$(jq -r --arg k "$LABEL_KEY" '[.data["initial-node-labels"][]? | select(.key == $k) | .value] | first // empty' <<<"$data")
  echo "  pool  $name  state=$state  shape=$shape ($ocpus OCPU, ${mem} GB)  k8s=$k8s"
  [ "$state" = ACTIVE ] && ok "pool is ACTIVE" || warn "pool state is $state"
  if [ "$size" = null ]; then
    warn "pool has no node-config-details.size (legacy placement); the controller needs node config details"
  elif [ "$size" = 0 ] && [ "$nodes" = 0 ]; then
    ok "size 0 and no nodes"
  else
    warn "size=$size, nodes=$nodes: the controller deletes every node in this pool that no scan owns"
  fi
  if [ "$label" = "$LABEL_VALUE" ]; then
    ok "initial node label ${LABEL_KEY}=${LABEL_VALUE} present"
  else
    warn "initial node label ${LABEL_KEY}=${LABEL_VALUE} missing (found '${label:-none}'): new nodes will not match WORKER_NODE_SELECTOR"
  fi
  cpu=$(jq -r --arg c "$class" '.[$c].cpu' <<<"$SIZE_CLASSES")
  memreq=$(jq -r --arg c "$class" '.[$c].memory' <<<"$SIZE_CLASSES")
  echo "  class requests cpu=$cpu memory=$memreq: make sure the shape above leaves room for them plus system pods"
done

echo
if [ "$rc" = 0 ]; then echo "preflight: all checks passed"; else echo "preflight: see WARN lines above"; fi
exit $rc
