# Virtual-node mode (`NODE_MODE=none` + an OKE virtual node pool)

OKE virtual nodes run each pod as serverless capacity: there is no worker VM
to create or delete, the pod is billed per second of the CPU and memory it
requests, and nothing runs when there is no scan. Isolation is per pod
rather than per VM.

Requirements (Oracle docs/blogs): an **Enhanced** cluster with **VCN-native
pod networking** and a virtual node pool. Some Kubernetes features are not
available on virtual nodes; check Oracle's "Virtual Nodes" limitations page
for your workload.

## Pointing a size class at virtual nodes

Virtual nodes carry their own labels and a taint. Oracle's examples use a
`type=virtual-node` selector with a toleration for the
`virtual-kubelet.io/provider` taint, but the exact keys on your cluster are
authoritative — read them off a virtual node:

```bash
kubectl get nodes -o custom-columns='NAME:.metadata.name,LABELS:.metadata.labels,TAINTS:.spec.taints' | grep -i virtual
```

Then define a class with that selector and toleration. Set
`WORKER_NODE_SELECTOR` to empty if the virtual node pool does not carry
`workload=worker`:

```text
WORKER_NODE_SELECTOR=""
SIZE_CLASSES='{"serverless":{"cpu":"2","memory":"8Gi",
  "nodeSelector":{"type":"virtual-node"},
  "tolerations":[{"key":"virtual-kubelet.io/provider","operator":"Exists","effect":"NoSchedule"}]}}'
DEFAULT_SIZE_CLASS=serverless
```

Requests equal limits in every class, which is what Oracle recommends on
virtual nodes (pods get assured capacity). `ONE_POD_PER_NODE` is irrelevant
here; leave it on.
