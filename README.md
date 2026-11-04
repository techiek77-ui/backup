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
| `MAX_CONCURRENT` | `10` | Max simultaneous worker pods; extra requests queue as `Pending`. |
| `MAX_LOG_BYTES` | `65536` | Log bytes kept per job. |
| `KEEP_FAILED_PODS` | `false` | Leave failed pods in place for debugging. |
| `PORT` | `8080` | HTTP port. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. |

## How a job runs

1. `POST /work` stores the job as `Pending` and starts a goroutine (queued if
   `MAX_CONCURRENT` pods are already running).
2. The goroutine creates a Pod `work-<id>-xxxxx` with `restartPolicy: Never`,
   `activeDeadlineSeconds`, the node selector/tolerations, labels
   `app.kubernetes.io/name=podspawner-worker` and `podspawner.io/job-id=<id>`,
   small resource requests/limits, and no service-account token.
3. It watches that single pod. Scheduling failures and image-pull errors are
   copied into the job's `message`; a pod that is never scheduled is given up
   after `WORKER_SCHEDULE_TIMEOUT` (`activeDeadlineSeconds` only starts once a
   kubelet has accepted the pod, so it would otherwise hang forever). Watches
   the API server closes are resumed from a fresh GET, so long jobs are safe.
4. On `Succeeded`/`Failed` it reads the container logs and exit code, records
   them, and **deletes the pod** (always — also on timeouts, errors and
   controller shutdown; the only exception is `KEEP_FAILED_PODS=true`).
5. On start-up the controller deletes any pod carrying its worker label that a
   previous instance left behind. The Deployment uses `strategy: Recreate` so
   two controllers never overlap.

RBAC is namespace-scoped: the controller may `create/get/list/watch/delete`
pods and `get` pod logs in its own namespace only.

## Known limitations (it is a PoC)

* Job state is in-memory: a controller restart forgets jobs and aborts in-flight
  work. Persist jobs, or model each request as a Kubernetes `Job`/CRD, for
  anything beyond a PoC.
* No authentication/authorization on the API, and callers choose the image and
  command — treat the endpoint as trusted-network only.
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
