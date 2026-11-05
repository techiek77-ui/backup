// Package nodes brings a dedicated worker node up for a job and tears it
// down afterwards. The Provisioner interface is what the spawner uses; the
// OKE implementation drives Oracle Container Engine node pools.
package nodes

import (
	"context"

	"github.com/techiek77-ui/backup/internal/config"
)

// Node is a node the controller brought up for one job.
type Node struct {
	ID     string // cloud identifier: the OCI instance OCID
	Name   string // Kubernetes node name, set once the node has joined
	PoolID string
	JobID  string
}

// Reporter receives human-readable progress for the job's message field.
type Reporter func(msg string)

// Provisioner brings up a dedicated node for a job and tears it down.
type Provisioner interface {
	// Provision returns once a fresh node for class is Ready in Kubernetes.
	// On error nothing is left behind that the next Sweep won't remove.
	Provision(ctx context.Context, jobID string, class config.SizeClass, report Reporter) (*Node, error)
	// Release tears the node down. Safe to call for a node already gone.
	Release(ctx context.Context, n *Node) error
	// Sweep removes nodes in the managed pools that no active job owns and
	// returns how many it asked the cloud to delete.
	Sweep(ctx context.Context, jobActive func(jobID string) bool) (int, error)
	// Pools lists the node pools the provisioner manages.
	Pools() []string
}
