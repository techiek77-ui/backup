#!/usr/bin/env bash
# End-to-end demo against whatever cluster kubectl currently points at
# (kind or OKE): port-forward the API, submit work, watch the worker pod
# appear on a labelled node, finish, and get deleted.
set -euo pipefail

NS="${NS:-podspawner}"
PORT="${PORT:-8080}"
IMAGE="${WORKER_IMAGE:-busybox:1.36}"
SLEEP="${WORK_SECONDS:-15}"
BASE="http://localhost:${PORT}"

command -v jq >/dev/null || { echo "this script needs jq" >&2; exit 1; }

kubectl -n "$NS" port-forward svc/podspawner "${PORT}:80" >/dev/null 2>&1 &
PF=$!
trap 'kill "$PF" 2>/dev/null || true' EXIT
for _ in $(seq 1 40); do
  curl -sf "$BASE/healthz" >/dev/null && break
  sleep 0.25
done
curl -sf "$BASE/healthz" >/dev/null || { echo "port-forward to svc/podspawner failed" >&2; exit 1; }

echo "==> Worker pods before:"
kubectl -n "$NS" get pods -l app.kubernetes.io/name=podspawner-worker -o wide 2>/dev/null || true

echo
echo "==> POST /work"
ID=$(curl -sf -X POST "$BASE/work" -H 'content-type: application/json' -d @- <<EOF | tee /dev/stderr | jq -r .id
{"image":"$IMAGE","command":["sh","-c","echo hello from \$(hostname); echo working for ${SLEEP}s; sleep ${SLEEP}; echo done"]}
EOF
)
echo
echo "==> job id: $ID"

sleep 3
echo
echo "==> Worker pod while running (note the NODE column):"
kubectl -n "$NS" get pods -l app.kubernetes.io/name=podspawner-worker -o wide

echo
echo "==> Polling GET /work/$ID"
while :; do
  JOB=$(curl -sf "$BASE/work/$ID")
  STATUS=$(jq -r .status <<<"$JOB")
  MSG=$(jq -r '.message // empty' <<<"$JOB")
  printf '   status=%s %s\n' "$STATUS" "${MSG:+($MSG)}"
  case "$STATUS" in Succeeded|Failed) break ;; esac
  sleep 2
done

echo
echo "==> Final job record:"
jq . <<<"$JOB"

echo
echo "==> Worker pods after (should be empty):"
kubectl -n "$NS" get pods -l app.kubernetes.io/name=podspawner-worker -o wide 2>/dev/null || true
