# Cluster Autoscaler mode (`NODE_MODE=none`)

The controller only creates the worker pod; the Kubernetes Cluster Autoscaler
sees it pending, scales the matching node pool up from 0, and removes the
node again once the pod is gone. The upstream OCI provider builds the
template for an empty pool from the pool's **initial node labels**, so the
pod's `nodeSelector` is enough to trigger the right pool.

## Node pools

One pool per size class, each with size 0, the shape for that class, and
initial node labels `workload=worker` **and** `size=<class>`. Keep a separate
"core" pool (labelled e.g. `workload=core`) that never scales to zero for
the controller and the autoscaler themselves.

Size classes then select pools by label:

```text
SIZE_CLASSES='{"small":{"cpu":"1","memory":"2Gi","nodeSelector":{"size":"small"}},
               "large":{"cpu":"8","memory":"32Gi","nodeSelector":{"size":"large"}}}'
```

`ONE_POD_PER_NODE=true` (default) adds a required pod anti-affinity, so the
autoscaler brings up one node per scan instead of packing scans together.

Raise `WORKER_SCHEDULE_TIMEOUT` (default 2m) above your measured
scale-up + boot time, e.g. `12m`, or jobs fail while the node is still
booting.

## IAM

Node pool scaling needs `manage cluster-node-pools`.

Instance principal (manifest default):

```text
# dynamic group, e.g. all instances in the cluster's compartment
ALL {instance.compartment.id = '<compartment-ocid>'}
```

```text
Allow dynamic-group <dynamic-group-name> to manage cluster-node-pools in compartment <compartment-name>
```

Workload identity (enhanced clusters; set the three `OCI_*` variables in the
manifest instead of `OKE_USE_INSTANCE_PRINCIPAL`):

```text
Allow any-user to manage cluster-node-pools in compartment <compartment-name> where all {
  request.principal.type = 'workload',
  request.principal.namespace = 'kube-system',
  request.principal.service_account = 'cluster-autoscaler',
  request.principal.cluster_id = '<cluster-ocid>'
}
```

## Install

```bash
kubectl apply -f deploy/addons/cluster-autoscaler/cluster-autoscaler.yaml
```

```bash
kubectl -n kube-system logs deploy/cluster-autoscaler -f
```

Enhanced clusters can run the autoscaler as an OKE add-on instead of this
Deployment; it takes the same `--nodes` and scale-down settings as add-on
configuration arguments (Oracle docs: "Using the Kubernetes Cluster
Autoscaler").

## Timeline and cost

Request → pod Pending → autoscaler scale-up (within its 10 s scan) → node
boots and joins (minutes) → pod runs → pod deleted → node empty →
`scale-down-unneeded-time` (1m here) → node deleted. The node is billed from
launch until termination, so with these flags roughly scan time + boot +
~1–2 minutes of tail per scan.

Sources: kubernetes/autoscaler `cluster-autoscaler/cloudprovider/oci/README.md`
and `examples/oci-nodepool-cluster-autoscaler-w-principals.yaml`.
