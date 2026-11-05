# Controller-managed nodes on OKE (`NODE_MODE=oke`)

Every scan gets a fresh node: the controller raises the size class's node
pool by one, waits for the new node to join and become Ready, pins the worker
pod to it, and when the pod is done deletes **that** node with
`DeleteNode(isDecrementSize=true, overrideEvictionGraceDuration=PT0M)`, which
also shrinks the pool back. No autoscaler is involved.

## 1. Node pools

Create one node pool per size class (`small`, `large`, ... as in
`SIZE_CLASSES`), each with:

* the shape/OCPU/memory for that class (worker pods request `cpu`/`memory`
  from the class; keep the node comfortably larger),
* **size 0**,
* initial node label `workload=worker` (`--initial-node-labels
  '[{"key":"workload","value":"worker"}]'`),
* an OKE image (fastest boot),
* **no** Cluster Autoscaler / Karpenter managing it,
* nothing else scheduled on it: the controller deletes every node in these
  pools that no active scan owns (on start-up and every `SWEEP_INTERVAL`).

Put the pool OCIDs into `SIZE_CLASSES` in `deployment-patch.yaml`.

Check the pools before deploying (size 0, label, state, legacy placement):

```bash
SIZE_CLASSES='<the JSON from deployment-patch.yaml>' make oke-preflight
```

## 2. IAM

The controller calls `GetNodePool`, `UpdateNodePool` (size) and `DeleteNode`.
All three are covered by `manage cluster-node-pools`.

The overlay ships `OCI_AUTH_MODE=auto`, which means Workload Identity
in-cluster. On a **Basic** cluster set `OCI_AUTH_MODE=instance-principal`
instead; everything else is the same.

**Enhanced cluster: Workload Identity** (recommended, no keys anywhere).
`OCI_AUTH_MODE=auto` or `workload-identity`, plus `OCI_RESOURCE_PRINCIPAL_REGION`:

```text
Allow any-user to manage cluster-node-pools in compartment <compartment-name> where all {
  request.principal.type = 'workload',
  request.principal.namespace = 'podspawner',
  request.principal.service_account = 'podspawner',
  request.principal.cluster_id = '<cluster-ocid>'
}
```

**Basic cluster: Instance Principal.** `OCI_AUTH_MODE=instance-principal`.
Dynamic group matching the nodes the controller runs on, e.g.
`ALL {instance.compartment.id = '<compartment-ocid>'}` (narrow it with a tag
if the compartment holds other instances), then:

```text
Allow dynamic-group <dynamic-group-name> to manage cluster-node-pools in compartment <compartment-name>
```

Any pod on those nodes can use the instance principal; keep the controller on
its own small pool if that matters.

**From a laptop** (PoC without any in-cluster identity): run the controller
outside the cluster with your CLI config, `OCI_AUTH_MODE=config-file`,
`KUBECONFIG` pointing at the OKE cluster and `POD_NAMESPACE=podspawner`:

```bash
NODE_MODE=oke OCI_AUTH_MODE=config-file POD_NAMESPACE=podspawner \
SIZE_CLASSES='{"small":{"cpu":"1","memory":"2Gi","nodePoolId":"ocid1.nodepool.oc1..."}}' DEFAULT_SIZE_CLASS=small \
go run ./cmd/controller
```

Worker pods still run in the cluster; the API is on `localhost:8080`.

## 3. Kubernetes RBAC

`deploy/base/rbac.yaml` already grants the controller read-only access to
`nodes` cluster-wide (to see when the new node is Ready) besides its
namespaced pod permissions.

## 4. Deploy

```bash
make oke-deploy OVERLAY=oke-managed-nodes IMAGE=<region-key>.ocir.io/<tenancy-namespace>/podspawner:0.1.0
```

Then submit a scan and watch the pool grow and shrink:

```bash
curl -s -X POST localhost:8080/work -H 'content-type: application/json' \
  -d '{"image":"busybox:1.36","command":["sh","-c","echo scanning; sleep 60"],"size":"small"}'
```

```bash
watch -n5 'kubectl get nodes -l workload=worker; kubectl -n podspawner get pods -o wide'
```

`GET /work/{id}` shows `Provisioning` with messages such as
`node …abcd is CREATING`, then `Running` with `nodeId`/`nodeName`, and after
completion the node disappears from `kubectl get nodes`.

## What to expect

* **Latency:** a scan starts after scale-up + instance boot + kubelet join,
  typically several minutes on OKE; measure it and set
  `NODE_PROVISION_TIMEOUT` with headroom.
* **Capacity errors:** if OKE cannot create the node (`Out of host capacity`,
  limits), the job fails within one poll with that message and the failed
  node is deleted; the pool returns to its previous size.
* **Concurrency:** size changes to a pool are serialised by the controller;
  N concurrent scans in one class become N nodes, claimed oldest-request
  first.
* **Leaks:** nodes whose job is gone are removed by the sweep; a controller
  restart empties the managed pools first (job state is in memory).

## Operations

* **What the controller owns right now:** `kubectl get nodes -l workload=worker`
  and `curl localhost:8080/work` (`nodeId`, `nodeName`, `nodeReadyAt`).
  `nodeReadyAt - createdAt` is the node lead time; set
  `NODE_PROVISION_TIMEOUT` to a multiple of the worst value you see.
* **A node that should be gone:** wait one `SWEEP_INTERVAL` (default 5 min);
  the sweep deletes every node in the managed pools that no active scan
  owns. To force it, restart the controller (its start-up sweep empties the
  pools). By hand:

  ```bash
  oci ce node-pool delete-node --node-pool-id <pool-ocid> --node-id <instance-ocid> --is-decrement-size true --override-eviction-grace-duration PT0M
  ```

* **`KEEP_FAILED_PODS=true`:** a failed scan keeps its pod **and its node**
  until you delete the pod and the sweep removes the node afterwards (or
  you delete the node as above). Debugging only; every kept node is billed.
* **Pool size drifted** (size > nodes, or a node you did not expect): the
  controller only ever changes size by `+N` on scale-up and by `-1` per node
  it deletes; anything else was done outside it. Correct the size in the
  Console or with `oci ce node-pool update --size`, then let the sweep run.
* **Changing a class's shape:** edit the node pool (new nodes get the new
  shape); no controller change is needed unless the pool OCID changes.
