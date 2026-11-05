# Karpenter mode (`NODE_MODE=none` + Karpenter Provider for OCI)

Oracle's Karpenter Provider for OCI (KPO, GA) launches an instance for each
pending pod that no existing node fits and removes it when it is empty, with
no node pool to pre-create. For scan-per-node:

* `scan-nodepools.yaml` defines a `NodePool` + `OCINodeClass` per size class,
  labelled `workload=worker,size=<class>`, with
  `consolidationPolicy: WhenEmpty` and `consolidateAfter: 30s`, so a node
  disappears about half a minute after its scan's pod is deleted.
* Size classes select them by label:

  ```text
  SIZE_CLASSES='{"small":{"cpu":"1","memory":"2Gi","nodeSelector":{"size":"small"}},
                 "large":{"cpu":"8","memory":"32Gi","nodeSelector":{"size":"large"}}}'
  ```

  The pod's `cpu`/`memory` request is what Karpenter sizes the instance by
  (within the class's `shapeConfigs`).
* `ONE_POD_PER_NODE=true` keeps one scan per instance.
* Raise `WORKER_SCHEDULE_TIMEOUT` above instance boot time (e.g. `12m`).

## Requirements (from the KPO docs)

* An OKE cluster with at least one existing managed node pool or self-managed
  node for KPO itself (and this controller) to run on; Oracle's examples use
  Enhanced clusters.
* IAM for KPO (workload identity, namespace/service account where KPO runs,
  default `default`/`karpenter`):

  ```text
  Allow any-user to manage instance-family in compartment <compartment-name> where all { request.principal.type = 'workload', request.principal.namespace = '<namespace>', request.principal.service_account = '<service-account>', request.principal.cluster_id = '<cluster-ocid>' }
  Allow any-user to manage volumes in compartment <compartment-name> where all { ...same conditions... }
  Allow any-user to manage volume-attachments in compartment <compartment-name> where all { ...same conditions... }
  Allow any-user to manage virtual-network-family in compartment <compartment-name> where all { ...same conditions... }
  Allow any-user to inspect compartments in compartment <compartment-name> where all { ...same conditions... }
  ```

* IAM for launched instances to join the cluster:

  ```text
  # dynamic group
  ALL {instance.compartment.id = '<compartment-ocid>'}
  ```

  ```text
  Allow dynamic-group <domain-name>/<dynamic-group-name> to {CLUSTER_JOIN} in compartment <compartment-name>
  ```

* Install KPO with its Helm chart following
  <https://github.com/oracle/karpenter-provider-oci> (docs/guide/installation-using-helm.md),
  then:

  ```bash
  kubectl apply -f deploy/addons/karpenter/scan-nodepools.yaml
  ```

Scheduling label keys documented by KPO: `oci.oraclecloud.com/instance-shape`,
`karpenter.sh/capacity-type`, `kubernetes.io/arch`, `kubernetes.io/os`,
`karpenter.sh/nodepool`. Custom labels come from
`NodePool.spec.template.metadata.labels`, which is how `size=<class>` gets
onto the nodes.
