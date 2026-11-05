# podspawner — on-demand worker pods on Kubernetes (PoC)

One always-running **controller pod** exposes a small HTTP API. Every work
request makes it **spawn a short-lived worker pod** on a **labelled node**,
wait for the pod to finish, collect the exit code and logs, and **delete the
pod**. Targets Oracle Container Engine for Kubernetes (OKE) managed node
pools; a local [kind](https://kind.sigs.k8s.io/) demo is included.

```
 client ──POST /work──▶ ┌──────────────────────────┐
                        │ controller (Deployment)  │── create Pod ──▶ ┌────────────────────┐
 client ◀─202 {id}───── │  • in-memory job store   │                  │ work-<id>-xxxxx    │
                        │  • one goroutine per job │◀── watch ─────── │ nodeSelector:      │
 client ──GET /work/id─▶│  • concurrency cap       │── get logs ────▶ │   workload=worker  │
 client ◀─status,logs── │                          │── delete Pod ──▶ │ restartPolicy:Never│
                        └──────────────────────────┘                  └────────────────────┘
                                                                        ▲ scheduled only on
                                                                        │ nodes labelled
                                                                        │ workload=worker
```

## Layout

| Path | Purpose |
| --- | --- |
| `cmd/controller` | entrypoint: config, k8s client, HTTP server, graceful shutdown |
| `internal/api` | HTTP handlers, job IDs, concurrency cap / queueing |
| `internal/spawner` | builds the worker Pod, watches it, fetches logs, deletes it |
| `internal/store` | in-memory job state |
| `internal/config` | env-var parsing |
| `deploy/base` | Namespace, RBAC, Deployment, ClusterIP Service (kustomize) |
| `deploy/overlays/kind` | local demo overlay |
| `deploy/overlays/oke` | OKE overlay: OCIR image, pull secret, node selector; optional internal OCI LB |
| `hack/` | `kind-up.sh` (cluster + label + build + deploy), `demo.sh` (end-to-end run) |

## API

| Method & path | Description |
| --- | --- |
| `POST /work` | Submit work. Body: `{"image":"busybox:1.36","command":["sh","-c","echo hi"],"args":[],"timeoutSeconds":120}`. `image` defaults to `WORKER_DEFAULT_IMAGE`. Returns `202 Accepted` with the job and a `Location` header. |
| `GET /work/{id}` | Job status (`Pending` → `Running` → `Succeeded`/`Failed`), pod + node name, exit code, logs, error, and an in-flight `message` such as `Unschedulable: 0/3 nodes are available …` or `ErrImagePull: …`. |
| `GET /work` | All jobs, newest first (logs omitted). |
| `GET /healthz`, `GET /readyz` | Probes. |

```bash
curl -s -X POST localhost:8080/work -H 'content-type: application/json' \
  -d '{"image":"busybox:1.36","command":["sh","-c","echo hello; sleep 10; echo done"]}'
```

```bash
curl -s localhost:8080/work/<id>
```

> The API has **no authentication**. It is exposed as a ClusterIP Service only;
> reach it with `kubectl port-forward` or put an authenticating gateway in front.

## Quick start on kind

Prerequisites: `docker`, `kind`, `kubectl`, `jq`, Go 1.24 (only for `make test`).

```bash
make test
```

```bash
make kind-up
```

`kind-up` creates a 3-node cluster, labels **only** `podspawner-worker` with
`workload=worker` (leaving `podspawner-worker2` unlabelled as a decoy), builds
the image, side-loads it, and deploys `deploy/overlays/kind`.

```bash
make demo
```

The demo port-forwards the Service, submits a 15-second job, shows the worker
pod while it runs (`NODE` column = the labelled node), polls until it finishes,
prints the final job record with logs, and shows the worker pod list is empty
again. Tear down with `make kind-down`.

## Deploy on OKE

The steps below were checked against Oracle's documentation (links at the end);
anything marked *optional* is not needed for the basic flow.

### 0. Access

Your `kubectl` context must be allowed to create Roles/RoleBindings. Members of
the tenancy **Administrators** group already can; anyone else needs the
Kubernetes `cluster-admin` ClusterRole bound to their user OCID by an admin:

```bash
kubectl create clusterrolebinding <name> --clusterrole=cluster-admin --user=<user-OCID>
```

### 1. Label the worker node pool

Worker pods carry `nodeSelector: {workload: worker}` (configurable through
`WORKER_NODE_SELECTOR`). Put that label on the node pool that should run them,
so every node the pool creates or replaces carries it automatically:

```bash
oci ce node-pool update --node-pool-id <node-pool-OCID> \
  --initial-node-labels '[{"key":"workload","value":"worker"}]'
```

This is the node pool's *Kubernetes labels* setting (`initialNodeLabels` in
the API: a JSON list of `key`/`value` objects; `oci ce node-pool update
--generate-param-json-input initial-node-labels` prints the exact template for
your CLI version). Three things to know:

* Treat `--initial-node-labels` as **replacing** the pool's label list, so
  include any labels the pool already has.
* It applies to **new nodes only**. Label the nodes that already exist (or
  cycle them with `--node-pool-cycling-details`):

  ```bash
  kubectl label nodes -l <some-label-identifying-the-pool> workload=worker
  ```

  or simply per node:

  ```bash
  kubectl label node <node-name> workload=worker
  ```

* The selector must be a node **label**. `oci.oraclecloud.com/node-pool-id` is
  an *annotation* on OKE nodes and cannot be used in a `nodeSelector`.
  Check what you have with `kubectl get nodes --show-labels`.

Verify before deploying — at least one node must come back:

```bash
kubectl get nodes -l workload=worker
```

### 2. Build and push the controller image to OCIR

Image path format: `<region-key>.ocir.io/<tenancy-namespace>/<repo>:<tag>`
(e.g. `fra.ocir.io/axabcdefghij/podspawner:0.1.0`). Log in with your OCI
username and an **auth token** (not your Console password):

```bash
docker login <region-key>.ocir.io -u '<tenancy-namespace>/<oci-username>'
```

Federated (IDCS) users: `<tenancy-namespace>/oracleidentitycloudservice/<username>`.

Build for the node pool's CPU architecture — `linux/amd64` for x86 shapes
(VM.Standard.E*, VM.Standard3…), `linux/arm64` for Ampere A1 pools — and push:

```bash
make oke-push IMAGE=<region-key>.ocir.io/<tenancy-namespace>/podspawner:0.1.0 PLATFORM=linux/amd64
```

### 3. Create the namespace and the OCIR pull secret

```bash
kubectl create namespace podspawner
```

```bash
kubectl -n podspawner create secret docker-registry ocirsecret \
  --docker-server=<region-key>.ocir.io \
  --docker-username='<tenancy-namespace>/<oci-username>' \
  --docker-password='<oci-auth-token>' \
  --docker-email='<email>'
```

(Skip the secret and remove `imagePullSecrets` from
`deploy/overlays/oke/deployment-patch.yaml` if the OCIR repository is public.)

### 4. Deploy

Either edit `images:` in `deploy/overlays/oke/kustomization.yaml` and run
`kubectl apply -k deploy/overlays/oke`, or let the Makefile substitute the image:

```bash
make oke-deploy IMAGE=<region-key>.ocir.io/<tenancy-namespace>/podspawner:0.1.0
```

```bash
kubectl -n podspawner get pods -o wide
```

```bash
kubectl -n podspawner logs deploy/podspawner
```

The start-up log line shows the effective `nodeSelector`, tolerations and
pull secrets. Then run the same demo as on kind — it works against whatever
cluster `kubectl` points at:

```bash
./hack/demo.sh
```

While it runs, `kubectl -n podspawner get pods -o wide` shows the worker pod on
one of the `workload=worker` nodes; afterwards it is gone.

### 5. Optional OKE settings

* **Worker images in a private OCIR repo** — set `WORKER_IMAGE_PULL_SECRET=ocirsecret`
  in `deployment-patch.yaml`; worker pods then reference that secret.
* **Dedicated (tainted) node pool** — if the worker nodes carry a taint such as
  `dedicated=worker:NoSchedule` (applied with `kubectl taint`, or at boot via a
  custom cloud-init that passes `--register-with-taints` to the kubelet), set
  `WORKER_TOLERATIONS='[{"key":"dedicated","operator":"Equal","value":"worker","effect":"NoSchedule"}]'`.
* **Reach the API from inside the VCN without port-forwarding** — apply
  `deploy/overlays/oke/service-internal-lb.yaml`. It creates an **internal**
  flexible OCI Load Balancer (`service.beta.kubernetes.io/oci-load-balancer-internal: "true"`,
  10 Mbps). Keep it internal: the API is unauthenticated.
* **Networking** — nodes pull OCIR images through the VCN's **service gateway**;
  pulling public images (the default `busybox:1.36` worker image comes from
  Docker Hub) from a private worker subnet needs a **NAT gateway** on its route
  table. If outbound internet is not allowed, mirror worker images into OCIR.
* **Scaling** — keep at least one node in the labelled pool, or jobs fail after
  `WORKER_SCHEDULE_TIMEOUT` with the scheduler's "0/N nodes are available"
  message. To pay for worker nodes only while work runs, let the OKE Cluster
  Autoscaler manage that pool with a zero minimum
  (`--nodes=0:<max>:<worker-node-pool-OCID>`) and raise
  `WORKER_SCHEDULE_TIMEOUT` to cover node provisioning time. Run the
  autoscaler and this controller on a pool that never scales to zero.

## Configuration (environment variables on the controller)

| Variable | Default | Meaning |
| --- | --- | --- |
| `POD_NAMESPACE` | `podspawner` | Namespace for worker pods (injected via downward API). |
| `WORKER_NODE_SELECTOR` | `workload=worker` | `k=v,k2=v2` node labels worker pods must match. Empty = any node. |
| `WORKER_TOLERATIONS` | – | JSON array of tolerations for worker pods. |
| `WORKER_IMAGE_PULL_SECRET` | – | Comma-separated secret names for worker image pulls. |
| `WORKER_SERVICE_ACCOUNT` | `podspawner-worker` | SA for worker pods (no permissions, token not mounted). |
| `WORKER_DEFAULT_IMAGE` | `busybox:1.36` | Used when a request omits `image`. |
| `WORKER_DEFAULT_TIMEOUT` | `10m` | Pod `activeDeadlineSeconds` when the request omits `timeoutSeconds`. |
| `WORKER_SCHEDULE_TIMEOUT` | `2m` | Fail the job if the pod is still unscheduled after this long. |
| `WORKER_IMAGE_PULL_TIMEOUT` | `3m` | Fail the job if its image is still unpullable (`ErrImagePull` / `ImagePullBackOff`) after this long; `InvalidImageName` fails at once. |
| `API_RETRY_WINDOW` | `2m` | Retry transient API-server errors (5xx, timeouts, connection failures) for this long before failing a job. |
| `SWEEP_INTERVAL` | `5m` | How often worker pods that no in-flight job owns are reaped. |
| `MAX_CONCURRENT` | `10` | Max simultaneous worker pods; extra requests queue as `Pending`. |
| `MAX_LOG_BYTES` | `65536` | Log bytes kept per job. |
| `KEEP_FAILED_PODS` | `false` | Leave failed pods in place for debugging. |
| `PORT` | `8080` | HTTP port. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. |

## How it works

```mermaid
sequenceDiagram
    autonumber
    participant C as Client
    participant K as Controller pod
    participant A as API server
    participant N as Labelled node
    C->>K: POST /work {image, command}
    K-->>C: 202 Accepted {id, status: Pending}
    K->>A: create Pod work-id-xxxxx (nodeSelector, restartPolicy Never, activeDeadlineSeconds)
    A->>N: scheduler binds to a workload=worker node; kubelet pulls image, runs it
    K->>A: GET + watch the pod (resumed if the watch drops)
    A-->>K: status events, copied to job.message (Unschedulable, ImagePullBackOff, ...)
    N-->>A: container exits: phase Succeeded or Failed
    A-->>K: terminal event
    K->>A: read logs, record exit code and node
    K->>A: delete Pod, confirm it is gone (force-delete if stuck Terminating)
    C->>K: GET /work/{id}
    K-->>C: {status, nodeName, exitCode, logs, error}
```

1. **Accept.** `POST /work` validates the body, stores the job as `Pending` and
   returns `202` at once. A goroutine takes a slot from the `MAX_CONCURRENT`
   semaphore; requests beyond the cap wait in memory as `Pending`.
2. **Create.** The goroutine creates one Pod `work-<id>-xxxxx` with
   `restartPolicy: Never`, `activeDeadlineSeconds`, the node selector and
   tolerations, the pull secrets, small resource requests/limits, labels
   `app.kubernetes.io/name=podspawner-worker` and `podspawner.io/job-id=<id>`,
   and no service-account token. Transient API errors are retried with
   back-off for `API_RETRY_WINDOW`; a retry first looks for a pod already
   carrying the job's label and adopts it, so a lost response never produces
   a duplicate pod.
3. **Watch.** The controller GETs the pod, then watches it from that resource
   version so no event is missed. Scheduling conditions and container
   `Waiting` reasons are copied to the job's `message`. Three things can end
   the wait early: no node after `WORKER_SCHEDULE_TIMEOUT`, an unpullable
   image after `WORKER_IMAGE_PULL_TIMEOUT` (`InvalidImageName` immediately),
   or the internal backstop 30 s past the pod's own deadline. A watch the API
   server closes, or that expires with `410 Gone`, is re-established from a
   fresh GET; API errors on the GET/watch are retried like the create.
4. **Collect.** On `Succeeded` or `Failed` the controller reads the container
   logs (capped at `MAX_LOG_BYTES`), the exit code and the node, and records
   the failure reason (`DeadlineExceeded`, `Evicted: ...`, `OOMKilled (exit
   code 137)`, `container exited with code N`).
5. **Delete.** Always, also on timeouts, errors and controller shutdown
   (exception: `KEEP_FAILED_PODS=true` keeps failed pods). The delete is
   graceful, then confirmed by polling for up to 45 s; a pod that is still
   `Terminating` after that, the signature of a node whose kubelet is gone,
   is force-deleted (`gracePeriodSeconds: 0`).
6. **Sweep.** On start-up and every `SWEEP_INTERVAL` the controller lists pods
   with the worker label and deletes any whose job is unknown or already
   finished (pods kept by `KEEP_FAILED_PODS` are left alone). Start-up
   therefore clears everything a previous instance left behind, and later
   sweeps catch the rare pod whose delete failed during an API-server outage.
7. **Boundaries.** RBAC is namespace-scoped: `create/get/list/watch/delete`
   on pods and `get` on `pods/log`, nothing else. The Deployment runs one
   replica with `strategy: Recreate` so two controllers never overlap (the
   new one's start-up sweep would otherwise delete the old one's pods).

## What happens when a node or node pool is unavailable

The controller never retries a job. Every scenario below ends with the job
`Failed`, an `error` that says why, and the worker pod removed; the only
variable is how long it takes, which these timers govern:

| Timer | Default | Fires when |
| --- | --- | --- |
| `WORKER_SCHEDULE_TIMEOUT` | 2 min | the pod still has no node |
| `WORKER_IMAGE_PULL_TIMEOUT` | 3 min | the pod has a node but its image cannot be pulled |
| `activeDeadlineSeconds` | request `timeoutSeconds`, else `WORKER_DEFAULT_TIMEOUT` (10 min) | the pod runs too long; enforced by the kubelet, so it counts only once the pod has started |
| internal backstop | deadline + 30 s | the kubelet never enforces the deadline (node lost after binding) |
| `API_RETRY_WINDOW` | 2 min | the API server keeps failing a create/get/watch/delete |
| delete confirmation | 45 s | a deleted pod is still `Terminating`; then it is force-deleted |
| `SWEEP_INTERVAL` | 5 min | catch-all for any worker pod the above did not remove |

### The pod cannot be placed

Cases: node pool not created yet, label missing or wrong, pool scaled to zero,
all nodes full (`Insufficient cpu/memory`), tainted pool without
`WORKER_TOLERATIONS`, nodes cordoned or NotReady, the autoscaler unable to add
nodes (OCI `Out of host capacity`, common on A1 shapes).

* `POST /work` still returns `202`: the API server accepts the Pod and
  scheduling is asynchronous. The job shows `Running` with an empty `nodeName`.
* Within seconds the scheduler sets `PodScheduled=False` and the job's
  `message` shows its reason, e.g. `Unschedulable: 0/3 nodes are available: 3
  node(s) didn't match Pod's node affinity/selector ...` or `... had untolerated
  taint {dedicated: worker}`.
* `activeDeadlineSeconds` does not tick for an unscheduled pod, which is why the
  schedule timer exists. After `WORKER_SCHEDULE_TIMEOUT` the job is `Failed`
  with `pod was not scheduled within 2m0s: Unschedulable: ...`, the Pending pod
  is deleted (instant, nothing to stop) and the concurrency slot is released.
* **Autoscaler from zero / scale-up (OKE):** the pending pod does trigger a
  scale-up because it has resource requests, but a new OKE node takes several
  minutes to boot and join. With the 2-minute default the job fails and the
  pod is deleted before the node is Ready, and the node may then sit idle until
  scale-down. Raise `WORKER_SCHEDULE_TIMEOUT` above the observed provisioning
  time when relying on this.
* **Back-pressure is queueing, not rejection:** with `MAX_CONCURRENT=10`, up to
  ten unschedulable pods occupy slots for the schedule timeout each; further
  requests queue in memory as `Pending`. The API never answers "no".

### The node disappears while the pod is running

* **Abrupt loss** (VM crash, preemptible instance reclaimed, node pool deleted
  without drain): the node goes NotReady after roughly 40–50 s; Kubernetes
  evicts the pod after the default 5-minute `not-ready`/`unreachable`
  toleration, but with no kubelet to confirm, the pod sits in `Terminating`.
  The controller keeps watching until the backstop (default ~10.5 min), then
  the job is `Failed` with `timed out after 10m30s waiting for pod to finish`
  and no logs. The delete is confirmed for 45 s and the stuck pod is then
  force-deleted. If the Node object is removed first (OKE's cloud controller
  deletes it once the instance is gone and pod GC reaps the pod), the
  controller sees the deletion instead and the error reads `pod was deleted
  externally`.
* **Graceful drain, OKE node cycling, node pool delete with drain:** the pod is
  evicted; the container gets SIGTERM, then SIGKILL after 30 s (a `sh -c ...;
  sleep` ignores SIGTERM). Depending on timing the controller catches either
  the terminal state (`container exited with code 137`, logs captured) or the
  deletion (`pod was deleted externally`, no logs). The pod is removed either
  way.
* **Node-pressure eviction** (`Evicted: The node was low on resource: memory`)
  and **OOMKilled** (`OOMKilled (exit code 137)`): the pod reaches `Failed`
  normally; the job records that reason with logs, and the pod is deleted.

### The controller itself

* **Controller node dies / controller restarted:** the Deployment reschedules
  it on any node (it has no node selector). In-memory jobs are lost, so
  `GET /work/{id}` for them returns `404`, and the start-up sweep deletes every
  worker pod carrying the label, including ones mid-run. `strategy: Recreate`
  keeps two controllers from overlapping.
* **Controller SIGTERM** (rollout, drain): in-flight jobs end `Failed: context
  canceled` and their pods are deleted; queued jobs end `Failed: controller
  shutting down before job started`.
* **API-server blips** (OKE control-plane maintenance, leader change): the
  create, GET, watch and delete calls are retried with back-off for
  `API_RETRY_WINDOW` before a job is failed. A delete that still fails is
  retried by the next sweep, so an outage cannot leak a running pod.

### Watching it happen

`GET /work/{id}` (`message` while in flight, `error` afterwards), the
controller log (`kubectl -n podspawner logs deploy/podspawner`), and
`kubectl -n podspawner get events --sort-by=.lastTimestamp`.

## Design notes

* **Bare Pods, not Jobs.** A Pod is the smallest unit and gives the controller
  direct control over the lifecycle it has to explain to the caller: it
  watches one object, reads its logs before deleting it, and has no TTL
  controller or retry policy acting behind its back. A `Job` would add
  `backoffLimit` retries and `ttlSecondsAfterFinished` cleanup that survive a
  controller restart; that is the first change to make when moving past a
  PoC.
* **A Deployment, not a StatefulSet, for the controller.** Nothing here needs a
  stable network identity or per-replica storage, so a one-replica Deployment
  with `Recreate` is the simplest correct shape. A StatefulSet becomes
  relevant when job state must survive restarts: a StatefulSet with a PVC
  (on OKE an OCI Block Volume via the default `oci-bv` StorageClass) holding
  an embedded database would let the controller re-adopt running pods after
  a restart instead of deleting them. The alternative is to represent each
  request as a custom resource, which also gives `kubectl get` visibility and
  needs no volume.
* **Workers are never StatefulSets.** Work items have no identity or storage to
  preserve; if one ever needs a volume, a `Job` with a PVC is the right tool.

## Known limitations (it is a PoC)

* Job state is in memory: a controller restart forgets jobs, aborts in-flight
  work and returns `404` for earlier IDs. Persist jobs (StatefulSet + PVC, or a
  CRD) for anything beyond a PoC; see Design notes.
* No authentication or authorization on the API, and callers choose the image
  and command: treat the endpoint as trusted-network only.
* Failed work is not retried automatically; the client resubmits.
* The request queue is unbounded and in memory.
* Logs are capped at `MAX_LOG_BYTES` and kept in memory.

## Sources consulted

* OCI CLI `oci ce node-pool update` (`--initial-node-labels`) — <https://docs.oracle.com/iaas/tools/oci-cli/latest/oci_cli_docs/cmdref/ce/node-pool/update.html>
* Updating an OKE node pool (labels apply to new nodes only; existing nodes via kubectl / cycling) — <https://docs.oracle.com/en-us/iaas/compute-cloud-at-customer/cmn/oke/updating-a-node-pool.htm>
* Pulling images from Registry during deployment (OCIR secret syntax) — <https://docs.oracle.com/iaas/Content/ContEng/Tasks/contengpullingimagesfromocir.htm>
* OCIR pull secrets for federated users — <https://docs.oracle.com/en/learn/oke-kyverno-imagepullsecrets/index.html>
* About access control and OKE (cluster-admin binding) — <https://docs.oracle.com/iaas/Content/ContEng/Concepts/contengaboutaccesscontrol.htm>
* OCI load balancer service annotations — <https://github.com/oracle/oci-cloud-controller-manager/blob/master/docs/load-balancer-annotations.md>
* Auto-tainting OKE nodes at start-up (cloud-init / `--register-with-taints`) — <https://blogs.oracle.com/cloud-infrastructure/post/auto-tainting-oke-nodes-startup>
* OKE worker nodes and OCIR via the service gateway — <https://docs.oracle.com/en/learn/oci-oke-cluster-worker-nodes>
* OCI CLI `oci ce node-pool create` (`--initial-node-labels` is a JSON list of `KeyValue`) — <https://docs.oracle.com/iaas/tools/oci-cli/latest/oci_cli_docs/cmdref/ce/node-pool/create.html>
* Using the Kubernetes Cluster Autoscaler on OKE — <https://docs.oracle.com/en-us/iaas/Content/ContEng/Tasks/contengusingclusterautoscaler.htm>
* Optimizing OKE costs with KEDA and Cluster Autoscaling (`--nodes=0:N:<ocid>`) — <https://blogs.oracle.com/cloud-infrastructure/optimizing-oke-costs-with-keda-and-cluster-autoscaling>
* OCI Load Balancer flexible shape bounds (10–8000 Mbps) — <https://docs.oracle.com/en-us/iaas/tools/python/latest/api/load_balancer/models/oci.load_balancer.models.ShapeDetails.html>
