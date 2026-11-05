# podspawner — on-demand worker pods on Kubernetes (PoC)

One always-running **controller pod** exposes a small HTTP API. Every work
request (a PII scan of one customer datasource, in the motivating use case)
makes it **bring up a node and spawn a short-lived worker pod on it**, wait
for the pod to finish, collect the exit code and logs, and **delete the pod
and the node**, so compute is paid for only while a scan runs. Targets
Oracle Container Engine for Kubernetes (OKE); a local
[kind](https://kind.sigs.k8s.io/) demo covers the pod lifecycle.

Four ways of getting the node are supported and documented in
[Node lifecycle modes](#node-lifecycle-modes): the controller driving OKE
node pools itself, the Cluster Autoscaler, Karpenter for OCI, or OKE virtual
nodes. Requests carry a **size class** (`small`, `large`, …) that decides the
pod's resources and which pool/shape serves it.

## Start here

The shortest path to a node that exists only while a scan runs
(controller-managed nodes on OKE); the other three modes are in
[Node lifecycle modes](#node-lifecycle-modes).

1. Local sanity check of the pod lifecycle (Docker, kind, kubectl, jq):
   `make kind-up && make demo`
2. On OKE, create one node pool per size class: size 0, the class's shape,
   an OKE image, initial node label `workload=worker`, no autoscaler.
3. Fill in `deploy/overlays/oke-managed-nodes/size-classes.yaml` (pool
   OCIDs, cpu/memory per class) and the region in its `deployment-patch.yaml`;
   grant the IAM policy from that overlay's README.
4. `make oke-preflight` — must print no `WARN`.
5. Push the image and deploy:
   `make oke-push oke-deploy OVERLAY=oke-managed-nodes IMAGE=<region-key>.ocir.io/<tenancy-namespace>/podspawner:0.1.0`
6. `SIZE=small ./hack/demo.sh` and watch `kubectl get nodes -l workload=worker`
   grow by one and shrink again.

Everything you will set is in the first table under
[Configuration](#configuration-environment-variables-on-the-controller);
the rest are tuning knobs.

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
| `internal/nodes` | `Provisioner` interface and the OKE implementation (grow pool, claim node, pin, delete node) |
| `internal/store` | in-memory job state |
| `internal/config` | env-var parsing |
| `deploy/base` | Namespace, RBAC, Deployment, ClusterIP Service (kustomize) |
| `deploy/overlays/kind` | local demo overlay |
| `deploy/overlays/oke` | OKE overlay, nodes provided by an autoscaler: OCIR image, pull secret, node selector; optional internal OCI LB |
| `deploy/overlays/oke-managed-nodes` | OKE overlay, controller-managed nodes (`NODE_MODE=oke`) with its IAM README |
| `deploy/addons` | Cluster Autoscaler manifest, Karpenter NodePools/OCINodeClasses, virtual-node notes |
| `hack/` | `kind-up.sh` (cluster + label + build + deploy), `demo.sh` (end-to-end run), `oke-preflight.sh` (node pool checks for oke mode) |

## API

| Method & path | Description |
| --- | --- |
| `POST /work` | Submit work. Body: `{"image":"busybox:1.36","command":["sh","-c","echo hi"],"args":[],"timeoutSeconds":120,"size":"small"}`. `image` defaults to `WORKER_DEFAULT_IMAGE`, `size` to `DEFAULT_SIZE_CLASS`, `timeoutSeconds` to `WORKER_DEFAULT_TIMEOUT` and may not exceed `WORKER_MAX_TIMEOUT`; an unknown size or an over-cap timeout is a `400` saying why. Returns `202 Accepted` with the job and a `Location` header. |
| `GET /work/{id}` | Job status (`Pending` → `Provisioning` (oke mode) → `Running` → `Succeeded`/`Failed`), pod name, node name and (oke mode) node OCID, exit code, logs, error, and an in-flight `message` such as `node …abcd is CREATING`, `Unschedulable: 0/3 nodes are available …` or `ErrImagePull: …`. |
| `GET /sizes` | The configured size classes and the default. |

Timestamps on a job: `createdAt`, `nodeReadyAt` (oke mode: node Ready),
`startedAt` (pod created), `endedAt`.
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
> Worker pods always run in the controller's own namespace (see Design notes
> for the cross-namespace variant).

## Prerequisites

What dynamic spawn-and-tear-down of pods from another pod needs, wherever the
cluster runs:

**Cluster**

* Any conformant Kubernetes cluster: OKE managed node pools, kind, and so on.
  The controller uses only the stable `core/v1` Pod API (create, get, list,
  watch, delete, logs), so it does not depend on a particular Kubernetes
  minor version, a CRD, or an add-on. The Go client library is pinned in
  `go.mod`.
* RBAC enabled (the default everywhere) and the API server reachable from pods
  at `kubernetes.default.svc`. The controller uses in-cluster configuration:
  the ServiceAccount token projected into its pod plus the
  `KUBERNETES_SERVICE_HOST`/`PORT` variables. If NetworkPolicies restrict
  egress in the namespace, allow the controller to reach the API server.
* At least one node carrying the worker label (`workload=worker` by default)
  that can run pods: Ready, not cordoned, tolerating any taint on it (see
  `WORKER_TOLERATIONS`), with room for the size class's `cpu`/`memory`
  (500m / 256Mi for the default class).
* Nodes able to pull the worker images (Docker Hub through a NAT gateway, or
  a mirror in OCIR with `WORKER_IMAGE_PULL_SECRET`) and the controller image
  (OCIR with `ocirsecret`).

**Permissions**

* Whoever applies the manifests must be allowed to create the Role and
  RoleBinding in the namespace. Kubernetes only lets you grant permissions you
  already hold, so this is a namespace admin or `cluster-admin`; on OKE a
  tenancy Administrator or a user bound to `cluster-admin` (step 0 below).
* The controller itself needs only what `deploy/base/rbac.yaml` grants:
  `pods` create/get/list/watch/delete and `pods/log` get in its own
  namespace, plus read-only `nodes` cluster-wide (to see when a provisioned
  node is Ready). No access to secrets.

**For controller-managed nodes (`NODE_MODE=oke`)**

* One OKE node pool per size class, size 0, initial node label
  `workload=worker`, not managed by any autoscaler and used by nothing else.
* OCI IAM: `manage cluster-node-pools` for the controller's identity (Workload
  Identity on Enhanced clusters, or an Instance Principal dynamic group), see
  `deploy/overlays/oke-managed-nodes/README.md`.
* Kubernetes RBAC: read-only `nodes` cluster-wide (in `deploy/base/rbac.yaml`).

**Namespace policies that interact with pod creation**

* `ResourceQuota`: worker pods always set requests and limits, so quotas that
  require them are satisfied. An exhausted quota makes the create fail with
  `403 Forbidden: exceeded quota ...`, which the job reports immediately.
* `LimitRange`: defaults apply to worker pods like to any pod; a maximum below
  500m CPU / 256Mi memory rejects them.
* Pod Security Admission: the worker pod spec satisfies the `baseline` level.
  Under `restricted` add a `securityContext` (non-root user, seccomp
  `RuntimeDefault`, all capabilities dropped) in `BuildPod` and use images
  that run as non-root. OKE enforces no level by default.
* Admission policies (Kyverno, Gatekeeper, `ValidatingAdmissionPolicy`) can
  mutate or reject worker pods; a rejection shows up as the job's `error`.

**Tooling** (to build and deploy; nothing extra is needed at runtime): Go
1.26 (any Go 1.21+ fetches it automatically), Docker (buildx for OKE images),
`kubectl`, and for the local demo `kind`
and `jq`.

## Quick start on kind

Prerequisites: `docker`, `kind`, `kubectl`, `jq`, Go 1.26 (only for `make test`).

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
`kubectl apply -k deploy/overlays/oke`, or let the Makefile substitute the image
(`OVERLAY=oke-managed-nodes` for controller-managed nodes; read that overlay's
README for the node pools and IAM it needs):

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

### Settings you will set

| Variable | Default | Meaning |
| --- | --- | --- |
| `NODE_MODE` | `none` | `none`: an autoscaler or virtual-node pool provides nodes; `oke`: the controller grows/shrinks OKE node pools itself for classes that have a `nodePoolId` (classes without one schedule normally, so VM and virtual-node classes can coexist). |
| `SIZE_CLASSES_FILE` | – | Path to a YAML or JSON file of size classes (the managed-nodes overlay mounts `size-classes.yaml` through a ConfigMap). Takes precedence over `SIZE_CLASSES`. |
| `SIZE_CLASSES` | one `default` class, 500m / 256Mi | Inline JSON alternative: class → `cpu`, `memory` (pod requests = limits), optional `nodeSelector`, `tolerations`, `nodePoolId` (oke mode). |
| `DEFAULT_SIZE_CLASS` | `default` | Class used when a request has no `size`. |
| `WORKER_NODE_SELECTOR` | `workload=worker` | `k=v,k2=v2` node labels worker pods must match; classes can add to it. Empty = any node. |
| `WORKER_DEFAULT_TIMEOUT` | `24h` | Pod `activeDeadlineSeconds` when the request omits `timeoutSeconds`. Scans are long and variable: send `timeoutSeconds` per request. |
| `WORKER_MAX_TIMEOUT` | `24h` | Cap for a request's `timeoutSeconds` (`400` above it); the default must not exceed it. |
| `MAX_CONCURRENT` | `10` | Simultaneous scans (each holds a node in oke mode); extra requests queue as `Pending`. |
| `OCI_AUTH_MODE` | `auto` | oke mode: `workload-identity`, `instance-principal`, `config-file`, or `auto` (workload identity in-cluster, config file outside). |
| `OCI_RESOURCE_PRINCIPAL_REGION` | – | Required with workload identity, e.g. `eu-frankfurt-1` (`OCI_REGION` is accepted too). |
| `WORKER_IMAGE_PULL_SECRET` | – | Comma-separated secret names for worker image pulls (private OCIR). |
| `WORKER_TOLERATIONS` | – | JSON array of tolerations for worker pods (tainted dedicated pool). |
| `WORKER_DEFAULT_IMAGE` | `busybox:1.36` | Used when a request omits `image`. |
| `POD_NAMESPACE` | `podspawner` | Namespace for worker pods; injected by the Deployment via the downward API. |

### Tuning knobs (defaults are fine)

| Variable | Default | Meaning |
| --- | --- | --- |
| `WORKER_SCHEDULE_TIMEOUT` | `2m` | Fail the job if the pod is still unscheduled after this long. Raise it (e.g. `12m`) when an autoscaler has to create the node first. |
| `WORKER_IMAGE_PULL_TIMEOUT` | `3m` | Fail the job if its image is still unpullable after this long; `InvalidImageName` fails at once. |
| `NODE_PROVISION_TIMEOUT` | `15m` | oke mode: scale-up + boot + join deadline per job. |
| `ONE_POD_PER_NODE` | `true` | Required pod anti-affinity: never two worker pods on one node. |
| `API_RETRY_WINDOW` | `2m` | Retry transient Kubernetes/OCI API errors for this long before failing a job. |
| `SWEEP_INTERVAL` | `5m` | How often worker pods (and, in oke mode, nodes) that no in-flight job owns are reaped. |
| `KEEP_FAILED_PODS` | `false` | Keep failed pods (and in oke mode their nodes) for debugging. |
| `MAX_LOG_BYTES` | `262144` | Log bytes kept per job. |
| `WORKER_LOG_TAIL_LINES` | `1000` | Keep the last N lines of the worker's log rather than the first bytes; `0` = from the start. |
| `WORKER_SERVICE_ACCOUNT` | `podspawner-worker` | SA for worker pods (no permissions, token not mounted). |
| `PORT` | `8080` | HTTP port. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. |

## How it works

```mermaid
sequenceDiagram
    autonumber
    participant C as Client
    participant K as Controller pod
    participant A as API server
    participant O as OCI (oke mode)
    participant N as Labelled node
    C->>K: POST /work {image, command, size}
    K-->>C: 202 Accepted {id, status: Pending}
    opt NODE_MODE=oke
        K->>O: UpdateNodePool(size+1) on the class's pool
        O-->>K: node CREATING → ACTIVE; Kubernetes Node Ready
    end
    K->>A: create Pod work-id-xxxxx (class resources, nodeSelector, anti-affinity, pinned to its node in oke mode)
    A->>N: scheduler binds to a workload=worker node; kubelet pulls image, runs it
    K->>A: GET + watch the pod (resumed if the watch drops)
    A-->>K: status events, copied to job.message (Unschedulable, ImagePullBackOff, ...)
    N-->>A: container exits: phase Succeeded or Failed
    A-->>K: terminal event
    K->>A: read logs, record exit code and node
    K->>A: delete Pod, confirm it is gone (force-delete if stuck Terminating)
    opt NODE_MODE=oke
        K->>O: DeleteNode(isDecrementSize=true, PT0M)
    end
    C->>K: GET /work/{id}
    K-->>C: {status, nodeName, exitCode, logs, error}
```

1. **Accept.** `POST /work` validates the body, stores the job as `Pending` and
   returns `202` at once. A goroutine takes a slot from the `MAX_CONCURRENT`
   semaphore; requests beyond the cap wait in memory as `Pending`.
2. **Create.** In `NODE_MODE=oke` the goroutine first brings up a node for
   the job's size class (job `Provisioning`; see Node lifecycle modes). It
   then creates one Pod `work-<id>-xxxxx` with `restartPolicy: Never`,
   `activeDeadlineSeconds`, the class's resources (requests = limits), the
   node selector and tolerations (global + class), a required anti-affinity
   against other worker pods, the pull secrets, labels
   `app.kubernetes.io/name=podspawner-worker`, `podspawner.io/job-id=<id>`
   and `podspawner.io/size=<class>`, no service-account token, and in oke
   mode a node affinity pinning it to its node. Transient API errors are retried with
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
   logs (the last `WORKER_LOG_TAIL_LINES` lines, capped at `MAX_LOG_BYTES`),
   the exit code and the node, and records
   the failure reason (`DeadlineExceeded`, `Evicted: ...`, `OOMKilled (exit
   code 137)`, `container exited with code N`).
5. **Delete.** Always, also on timeouts, errors and controller shutdown
   (exception: `KEEP_FAILED_PODS=true` keeps failed pods). The delete is
   graceful, then confirmed by polling for up to 45 s; a pod that is still
   `Terminating` after that, the signature of a node whose kubelet is gone,
   is force-deleted (`gracePeriodSeconds: 0`). In oke mode the job's node is
   deleted right after the pod.
6. **Sweep.** On start-up and every `SWEEP_INTERVAL` the controller lists pods
   with the worker label and deletes any whose job is unknown or already
   finished (pods kept by `KEEP_FAILED_PODS` are left alone). Start-up
   therefore clears everything a previous instance left behind, and later
   sweeps catch the rare pod whose delete failed during an API-server outage.
7. **Boundaries.** Pod RBAC is namespace-scoped: `create/get/list/watch/delete`
   on pods and `get` on `pods/log`; the only cluster-wide grant is read-only
   `nodes`. The Deployment runs one
   replica with `strategy: Recreate` so two controllers never overlap (the
   new one's start-up sweep would otherwise delete the old one's pods).

## Node lifecycle modes

Only one thing differs between the four modes: **who creates and deletes the
node**. The pod lifecycle above is identical, and in every mode a size
class decides the pod's `cpu`/`memory` (requests = limits) and the labels
that route it, and `ONE_POD_PER_NODE` guarantees a scan never shares a node.

| Mode | Node created by | Node removed | Config | Credentials | Cluster |
| --- | --- | --- | --- | --- | --- |
| **Controller-managed** (`NODE_MODE=oke`) | the controller: `UpdateNodePool(size+1)` on the class's pool | the controller, right after the pod: `DeleteNode(isDecrementSize, PT0M)` | pool OCID per class, `deploy/overlays/oke-managed-nodes` | `manage cluster-node-pools` via Workload Identity or Instance Principal | any (Workload Identity needs Enhanced) |
| **Cluster Autoscaler** (`NODE_MODE=none`) | the autoscaler, from the pending pod; pool min 0 | the autoscaler, `scale-down-unneeded-time` after the pod is gone (1m in our manifest) | pool per class with initial labels `workload=worker,size=<class>`; `deploy/addons/cluster-autoscaler` | autoscaler's own IAM | any |
| **Karpenter for OCI** (`NODE_MODE=none`) | Karpenter, launching an instance that fits the pod | Karpenter, `consolidateAfter` (30s) once empty | `NodePool`/`OCINodeClass` per class; `deploy/addons/karpenter` | KPO's own IAM | Enhanced (per Oracle's examples) |
| **Virtual nodes** (`NODE_MODE=none`) | nobody: the pod itself is the billed unit | nobody | class with the virtual-node selector/toleration; `deploy/addons/virtual-nodes` | none | Enhanced + VCN-native networking |

**Which to pick.** Controller-managed gives the literal "node up → scan →
node down" with the smallest idle tail and a fresh VM per scan; it is the
most code and needs OCI credentials in the controller. Cluster Autoscaler is
the standard Kubernetes answer with no cloud credentials in this controller
and an idle tail of a minute or two. Karpenter is the newest and the most
flexible about shapes. Virtual nodes remove node management entirely but
trade VM-level isolation for pod-level isolation and have feature limits.
All four are wired; switching is configuration.

### Size classes

```text
SIZE_CLASSES='{"small":{"cpu":"1","memory":"2Gi","nodeSelector":{"size":"small"},"nodePoolId":"ocid1.nodepool.oc1..."},
               "large":{"cpu":"8","memory":"32Gi","nodeSelector":{"size":"large"},"nodePoolId":"ocid1.nodepool.oc1..."}}'
DEFAULT_SIZE_CLASS=small
```

The same structure as YAML in a file (`SIZE_CLASSES_FILE`) is what the
managed-nodes overlay uses, mounted from a ConfigMap, so nobody edits JSON
inside a YAML string:

```yaml
small:
  cpu: 1
  memory: 2Gi
  nodePoolId: ocid1.nodepool.oc1...
large:
  cpu: 8
  memory: 32Gi
  nodePoolId: ocid1.nodepool.oc1...
```

A request names one with `"size":"large"`. `GET /sizes` lists them. Class
`nodeSelector`/`tolerations` are merged over the global
`WORKER_NODE_SELECTOR`/`WORKER_TOLERATIONS`; `nodePoolId` is used only in
oke mode (at least one class needs it there).

### Controller-managed nodes, step by step (`NODE_MODE=oke`)

1. `POST /work` → job `Provisioning`. The controller registers the job as
   waiting on the class's pool and raises the pool's size to cover every
   waiting job. One size conversation per pool runs at a time, and jobs that
   arrive while an update is being retried (OCI 409/429/5xx, e.g. while the
   pool is `UPDATING`) are coalesced into the next update as `+N`, the way
   the Cluster Autoscaler batches pending pods. `message`: `node pool scaled to 1; waiting for a new
   node`.
2. It polls the pool. A node that is `CREATING`/`ACTIVE`, not owned by another
   job, not being deleted, and not one that existed before this controller
   started is claimed by the **oldest waiting job**. `message`: `node …abcd
   is CREATING`.
3. Once OKE reports the node `ACTIVE`, the controller waits for the matching
   Kubernetes `Node` (`providerID` = instance OCID) to be Ready, schedulable
   and initialised by the cloud controller. `message`: `… waiting for it to
   join the cluster and become Ready`. If OKE instead reports a
   `nodeError` (e.g. `Out of host capacity`), the job fails right away with
   that message and the failed node is deleted, shrinking the pool again.
4. The worker pod is created pinned to that node (`nodeAffinity` on
   `metadata.name`) plus the class selector; job → `Running` with `nodeId`
   and `nodeName`.
5. Pod finishes → logs/exit code recorded → pod deleted → **node deleted**
   with `isDecrementSize=true` and `overrideEvictionGraceDuration=PT0M`
   (nothing left to drain), so the pool returns to its previous size.
6. Sweeps: at start-up every node in the managed pools is deleted (job state
   is in memory, nothing can be collected), and every `SWEEP_INTERVAL` any
   node no active job owns is deleted. The pools must therefore be used by
   nothing else.

Setup (pools, IAM for Workload Identity / Instance Principal, running the
controller from a laptop with `OCI_AUTH_MODE=config-file`) is in
`deploy/overlays/oke-managed-nodes/README.md`.

#### First-run checklist (controller-managed nodes)

1. **Pools:** one node pool per size class, size 0, the class's shape, an OKE
   image, initial node label `workload=worker`, not autoscaled, nothing else
   on it. Note each pool OCID. (In the Console the node pool form has these
   fields; with the CLI, `oci ce node-pool create --generate-full-command-json-input`
   prints the full template.)
2. **IAM:** the `manage cluster-node-pools` policy for the controller's
   identity (overlay README, section 2). On a Basic cluster set
   `OCI_AUTH_MODE=instance-principal` in the overlay.
3. **Overlay:** put the pool OCIDs and per-class cpu/memory into
   `deploy/overlays/oke-managed-nodes/size-classes.yaml`, the region into
   `OCI_RESOURCE_PRINCIPAL_REGION` in `deployment-patch.yaml`, and the OCIR
   image into `kustomization.yaml`; `ocirsecret` as in step 3 of Deploy on OKE.
4. **Preflight:** `make oke-preflight` (reads that file) must print no `WARN`
   lines.
5. **Deploy:** `make oke-deploy OVERLAY=oke-managed-nodes IMAGE=...`, then
   check the start-up log line for `controller-managed nodes enabled` with
   your pools and auth mode.
6. **First scan:** `SIZE=small ./hack/demo.sh` and, in another terminal,
   `watch -n5 'kubectl get nodes -l workload=worker; kubectl -n podspawner get pods -o wide'`.
   Expect `Provisioning` for a few minutes, `Running` on the new node, then
   the node gone.
7. **Tune:** read `nodeReadyAt - createdAt` from `GET /work/{id}` and set
   `NODE_PROVISION_TIMEOUT` to a comfortable multiple; then run a real scan
   with its own `timeoutSeconds`.

### Autoscaler-provided nodes (`NODE_MODE=none`)

The pod carries `nodeSelector` (global + class), the class resources and the
anti-affinity; the pod sits `Pending` until the autoscaler's node is Ready.
Two settings matter: raise `WORKER_SCHEDULE_TIMEOUT` above scale-up + boot
time (the default 2 min is for pools that already have nodes), and keep the
controller and the autoscaler on a pool that never scales to zero. Manifests,
flags and IAM: `deploy/addons/cluster-autoscaler`, `deploy/addons/karpenter`,
`deploy/addons/virtual-nodes`.

### Cost shape per scan

| Mode | Billed compute per scan ≈ |
| --- | --- |
| Controller-managed | boot + scan + termination (seconds) |
| Cluster Autoscaler | boot + scan + `scale-down-unneeded-time` (+ `scale-down-delay-after-add` if it is longer) |
| Karpenter | boot + scan + `consolidateAfter` |
| Virtual nodes | scan only (pod CPU/memory per second) |

OCI bills compute per second with a one-minute minimum; the boot phase
(several minutes on OKE managed nodes, fastest with OKE images) is paid in the
first three modes. For short scans on a shared pool, packing scans together
(`ONE_POD_PER_NODE=false`) would be cheaper than a node per scan; it was
ruled out here for tenant isolation.

## What happens when a node or node pool is unavailable

The controller never retries a job. Every scenario below ends with the job
`Failed`, an `error` that says why, and the worker pod removed; the only
variable is how long it takes, which these timers govern:

| Timer | Default | Fires when |
| --- | --- | --- |
| `WORKER_SCHEDULE_TIMEOUT` | 2 min | the pod still has no node |
| `WORKER_IMAGE_PULL_TIMEOUT` | 3 min | the pod has a node but its image cannot be pulled |
| `activeDeadlineSeconds` | request `timeoutSeconds`, else `WORKER_DEFAULT_TIMEOUT` (24 h) | the pod runs too long; enforced by the kubelet, so it counts only once the pod has started |
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
  The controller keeps watching until the backstop (the request's timeout
  + 30 s; with the 24 h default that is a long wait, another reason to send
  `timeoutSeconds` per scan), then the job is `Failed` with `timed out after
  … waiting for pod to finish` and no logs. The delete is confirmed for 45 s and the stuck pod is then
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

### Controller-managed nodes (`NODE_MODE=oke`)

* **OKE cannot create the node** (`Out of host capacity`, service limits,
  quota): the job fails on the next poll with OKE's `nodeError` message, the
  failed node is deleted with decrement, pool size is restored. Resubmit
  later or use another class/shape.
* **Node never becomes Ready** (boot or join problem): after
  `NODE_PROVISION_TIMEOUT` the job fails with `node … did not become Ready`
  and the node is deleted.
* **No node appears at all** after the scale-up (OKE accepted the size change
  but created nothing): the job fails with `no new node appeared in node pool
  … within 15m`; if a node shows up later, the next sweep deletes it.
* **OCI API errors**: `GetNodePool`/`UpdateNodePool`/`DeleteNode` are retried
  on 409/429/5xx for `API_RETRY_WINDOW`; a node whose delete still failed is
  removed by the next sweep.
* **Controller restart**: the start-up sweep deletes every node in the managed
  pools, including nodes of scans that were running (their jobs are gone with
  the memory). Scans in flight at restart are lost in every mode; here their
  nodes are also reclaimed instead of lingering.

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
* **Is this the usual way to do it?** Yes, for the mechanism: an in-cluster
  process authenticating with a ServiceAccount, scoped by RBAC, that creates a
  pod per unit of work, watches it, collects its logs and deletes it is how
  the Jenkins Kubernetes plugin, the GitLab Runner Kubernetes executor,
  Airflow's `KubernetesPodOperator`, Argo Workflows and Tekton run their work;
  a Spark driver creates its executor pods the same way. Three things separate
  this PoC from those production controllers: they persist state (a database
  or custom resources) rather than memory; most use informers (one shared
  watch over all their pods with a local cache) or controller-runtime instead
  of one watch per pod, which matters once hundreds of pods run at once; and
  the ones that scale out run several replicas with leader election. None of
  that changes the pod lifecycle described above. If a custom API is not a
  requirement, two adopted off-the-shelf shapes cover the same need: a
  Kubernetes `Job` per request, or KEDA's `ScaledJob` when requests arrive on
  a queue (it creates a Job per message and scales to zero with no controller
  code). For a long-running service that should grow with load the adopted
  mechanism is different again: a Deployment with the Horizontal Pod
  Autoscaler or KEDA, not a pod per request.
* **One namespace.** The controller creates, watches, sweeps and deletes worker
  pods in its own namespace only (`POD_NAMESPACE`, injected from the pod's
  metadata), and its pod RBAC is a namespaced `Role` (the only cluster-wide
  grant is read-only `nodes`). Everything the workers rely
  on, the `podspawner-worker` ServiceAccount, pull secrets, quotas and
  policies, lives in that namespace. To run workers in other namespaces you
  need, per target namespace: a `RoleBinding` granting the controller's
  ServiceAccount the same verbs (bind one `ClusterRole` through per-namespace
  `RoleBinding`s rather than a `ClusterRoleBinding`, so the grant stays
  limited to namespaces you chose), the worker ServiceAccount and pull
  secrets, and headroom in that namespace's quota and policies. In the code:
  an optional `namespace` field on the request checked against an allowlist,
  the spawner taking the namespace from the job, and one sweep per allowed
  namespace. A `ClusterRoleBinding` is the quick alternative, but it gives the
  controller pod rights in every namespace, including ones it should never
  touch.

## Verification status

Be aware of what has and has not been exercised:

* **Unit-tested against fakes** (`make test`, race detector on): the pod
  lifecycle against client-go's fake API server (success, failure, timeouts,
  image-pull errors, external deletion, watch drops, API outages, sweeps),
  the OKE provisioner against a fake Container Engine API (scale-up,
  claiming oldest-first, coalesced updates under 409, node errors,
  provisioning timeouts, idempotent deletes, sweeps, readiness checks), size
  classes, the API, and configuration.
* **Statically validated:** every rendered overlay and the add-on manifests
  pass `kubeconform -strict` against Kubernetes 1.33 schemas (Karpenter CRDs
  excepted); the controller cross-compiles for `linux/amd64` and
  `linux/arm64`; OKE/OCI facts are sourced in "Sources consulted".
* **Not yet run on a live cluster** by the author: neither the kind demo nor
  any OKE mode. First real runs to do, in order: `make kind-up && make demo`
  (pod lifecycle), then `NODE_MODE=none` on OKE with an existing labelled
  pool, then `NODE_MODE=oke` after `make oke-preflight`. Measure the node
  lead time (`nodeReadyAt - createdAt`) before relying on the defaults.

## Known limitations (it is a PoC)

* Job state is in memory: a controller restart forgets jobs, aborts in-flight
  work and returns `404` for earlier IDs. With hours-long scans this is the
  limitation that bites first: a controller rollout or node drain aborts
  every scan in flight (and in oke mode reclaims their nodes). Do not roll
  the controller while scans run, and persist jobs (StatefulSet + PVC, or a
  CRD that survives restarts) before relying on this in production; see
  Design notes.
* No authentication or authorization on the API, and callers choose the image
  and command: treat the endpoint as trusted-network only.
* Failed work is not retried automatically; the client resubmits.
* In `NODE_MODE=oke` the managed node pools belong to the controller: any
  node in them that no active scan owns is deleted, and a controller restart
  empties them. Size changes are one conversation per pool (concurrent scans
  are coalesced into one update), sized for tens of concurrent scans per
  class, not thousands. Pools must use node config details (placement
  configs), which every pool created in recent years does.
* A node per scan means every scan pays the node boot time (minutes on OKE);
  virtual nodes avoid it at the price of weaker isolation.
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
* Cluster Autoscaler OCI provider (node pools, scale from zero, IAM, env) — <https://github.com/kubernetes/autoscaler/blob/master/cluster-autoscaler/cloudprovider/oci/README.md> and its node-pool example manifest; template built from `InitialNodeLabels` in `cloudprovider/oci/nodepools/oci_manager.go`
* Deleting worker nodes (`DeleteNode`, `isDecrementSize`, eviction grace) — <https://docs.oracle.com/en-us/iaas/Content/ContEng/Tasks/contengdeletingworkernodes.htm>
* OCI Go SDK `containerengine` (`UpdateNodePool`, `DeleteNode`, node lifecycle states) and `common/auth` (`OkeWorkloadIdentityConfigurationProvider`, `InstancePrincipalConfigurationProvider`) — <https://github.com/oracle/oci-go-sdk>
* Granting workloads access to OCI resources (Workload Identity, policy syntax) — <https://docs.cloud.oracle.com/en-us/iaas/Content/ContEng/Tasks/contenggrantingworkloadaccesstoresources.htm>
* Karpenter Provider for OCI: usage (NodePool/OCINodeClass), IAM — <https://github.com/oracle/karpenter-provider-oci>; Oracle docs <https://docs.oracle.com/en-us/iaas/Content/ContEng/Tasks/conteng-kpo.htm>
* OKE virtual nodes (per-pod resources, limits) — <https://docs.oracle.com/en-us/iaas/Content/ContEng/Tasks/contengvirtualnodepodresourceallocation.htm>; mixed clusters with virtual nodes — <https://blogs.oracle.com/cloud-infrastructure/mixed-oke-cluster-karpenter-and-virtual-nodes>
