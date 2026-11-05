package nodes

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/containerengine"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/techiek77-ui/backup/internal/config"
)

// OKEAPI is the subset of the OCI Container Engine client the provisioner
// uses. *containerengine.ContainerEngineClient satisfies it; tests use a fake.
type OKEAPI interface {
	GetNodePool(context.Context, containerengine.GetNodePoolRequest) (containerengine.GetNodePoolResponse, error)
	UpdateNodePool(context.Context, containerengine.UpdateNodePoolRequest) (containerengine.UpdateNodePoolResponse, error)
	DeleteNode(context.Context, containerengine.DeleteNodeRequest) (containerengine.DeleteNodeResponse, error)
}

// OKEConfig tunes the OKE provisioner.
type OKEConfig struct {
	// Pools are the node pools this controller owns exclusively, one per size
	// class. Every node in them that no active job claims is deleted by Sweep.
	Pools []string
	// ProvisionTimeout bounds scale-up + boot + join. Default 15m.
	ProvisionTimeout time.Duration
	// PollInterval between node pool reads while waiting. Default 10s.
	PollInterval time.Duration
	// APIRetryWindow bounds retries of transient OCI errors. Default 2m.
	APIRetryWindow time.Duration

	retryBase time.Duration // tests
}

// OKE provisions one fresh OKE node per job by growing the class's node pool
// and deletes exactly that node afterwards with DeleteNode(isDecrementSize).
type OKE struct {
	api  OKEAPI
	kube kubernetes.Interface
	cfg  OKEConfig
	log  *slog.Logger

	mu         sync.Mutex
	claimed    map[string]*Node       // node ID -> owning job
	doomed     map[string]struct{}    // node IDs we asked OKE to delete
	foreign    map[string]struct{}    // node IDs present when a pool was first seen: never ours
	seen       map[string]bool        // pool ID -> first observation done
	scaleMu    map[string]*sync.Mutex // pool ID -> one size conversation with OCI at a time
	unaccepted map[string]int         // pool ID -> waiters not yet covered by an accepted size update
	waiters    map[string][]*waiter   // pool ID -> jobs waiting for a node, oldest first
}

type waiter struct {
	jobID    string
	ch       chan *Node
	accepted bool // a size update covering this waiter was accepted by OCI
}

func NewOKE(api OKEAPI, kube kubernetes.Interface, cfg OKEConfig, log *slog.Logger) *OKE {
	if cfg.ProvisionTimeout <= 0 {
		cfg.ProvisionTimeout = 15 * time.Minute
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 10 * time.Second
	}
	if cfg.APIRetryWindow <= 0 {
		cfg.APIRetryWindow = 2 * time.Minute
	}
	if cfg.retryBase <= 0 {
		cfg.retryBase = time.Second
	}
	if log == nil {
		log = slog.Default()
	}
	return &OKE{
		api: api, kube: kube, cfg: cfg, log: log,
		claimed: map[string]*Node{}, doomed: map[string]struct{}{}, foreign: map[string]struct{}{},
		seen: map[string]bool{}, waiters: map[string][]*waiter{},
		scaleMu: map[string]*sync.Mutex{}, unaccepted: map[string]int{},
	}
}

func (p *OKE) Pools() []string {
	out := append([]string(nil), p.cfg.Pools...)
	sort.Strings(out)
	return out
}

// Provision grows the class's node pool by one, waits for a node nobody else
// owns to appear, and then for it to become Ready in Kubernetes.
func (p *OKE) Provision(ctx context.Context, jobID string, class config.SizeClass, report Reporter) (*Node, error) {
	poolID := class.NodePoolID
	if poolID == "" {
		return nil, errors.New("size class has no nodePoolId")
	}
	if report == nil {
		report = func(string) {}
	}
	ctx, cancel := context.WithTimeout(ctx, p.cfg.ProvisionTimeout)
	defer cancel()
	log := p.log.With("job", jobID, "pool", shortID(poolID))

	w := &waiter{jobID: jobID, ch: make(chan *Node, 1)}
	p.mu.Lock()
	p.waiters[poolID] = append(p.waiters[poolID], w)
	p.unaccepted[poolID]++
	p.mu.Unlock()

	size, err := p.ensureScaled(ctx, poolID)
	if err != nil {
		p.dropWaiter(poolID, w)
		return nil, fmt.Errorf("scale up node pool: %w", err)
	}
	if size > 0 {
		log.Info("node pool scaled up", "size", size)
		report(fmt.Sprintf("node pool scaled to %d; waiting for a new node", size))
	} else {
		report("waiting for a new node (covered by an earlier scale-up)")
	}

	node, err := p.awaitClaim(ctx, poolID, w, report)
	if err != nil {
		return nil, err
	}
	log = log.With("node", shortID(node.ID))
	log.Info("node claimed")

	name, err := p.awaitReady(ctx, node, report)
	if err != nil {
		// Nothing may be left behind: tear the node down again.
		if rerr := p.Release(context.Background(), node); rerr != nil {
			log.Error("failed to delete node after provisioning error", "err", rerr)
		}
		return nil, err
	}
	node.Name = name
	log.Info("node ready", "k8sNode", name)
	return node, nil
}

// ensureScaled raises the pool's desired size to cover every waiter that no
// accepted size update covers yet. One conversation per pool at a time: jobs
// that arrive while the pool is busy (OKE answers 409 while a node pool is
// UPDATING) are coalesced into the next update, exactly like the Cluster
// Autoscaler batches pending pods, instead of each waiting its turn for a
// multi-minute work request. Returns the size requested, or 0 when an
// earlier update already covered this caller.
func (p *OKE) ensureScaled(ctx context.Context, poolID string) (int, error) {
	lock := p.poolLock(poolID)
	lock.Lock()
	defer lock.Unlock()

	delay := p.cfg.retryBase
	last := 0
	for attempt := 1; ; attempt++ {
		p.mu.Lock()
		n := p.unaccepted[poolID]
		p.mu.Unlock()
		if n == 0 {
			return last, nil
		}
		np, err := p.getPool(ctx, poolID)
		if err != nil {
			return 0, err
		}
		p.mu.Lock()
		p.observeLocked(poolID, np)
		p.mu.Unlock()

		size := desiredSize(np) + n
		_, err = p.api.UpdateNodePool(ctx, containerengine.UpdateNodePoolRequest{
			NodePoolId: common.String(poolID),
			UpdateNodePoolDetails: containerengine.UpdateNodePoolDetails{
				NodeConfigDetails: &containerengine.UpdateNodePoolNodeConfigDetails{Size: common.Int(size)},
			},
			RequestMetadata: requestMetadata(),
		})
		if err == nil {
			p.mu.Lock()
			accepted := 0
			for _, w := range p.waiters[poolID] {
				if accepted == n {
					break
				}
				if !w.accepted {
					w.accepted = true
					accepted++
				}
			}
			if p.unaccepted[poolID] -= accepted; p.unaccepted[poolID] < 0 {
				p.unaccepted[poolID] = 0
			}
			p.mu.Unlock()
			last = size
			delay = p.cfg.retryBase
			continue // newcomers may have registered meanwhile
		}
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		if !isTransient(err) {
			return 0, fmt.Errorf("UpdateNodePool: %w", err)
		}
		p.log.Warn("node pool busy or OCI error; retrying size update", "pool", shortID(poolID), "attempt", attempt, "in", delay, "err", err)
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(delay):
		}
		if delay *= 2; delay > 15*time.Second {
			delay = 15 * time.Second
		}
	}
}

func (p *OKE) poolLock(poolID string) *sync.Mutex {
	p.mu.Lock()
	defer p.mu.Unlock()
	m, ok := p.scaleMu[poolID]
	if !ok {
		m = &sync.Mutex{}
		p.scaleMu[poolID] = m
	}
	return m
}

// awaitClaim polls the pool until a node is assigned to this waiter.
// Assignment is first come, first served in scale-up order, whichever
// waiter happens to observe the new node.
func (p *OKE) awaitClaim(ctx context.Context, poolID string, w *waiter, report Reporter) (*Node, error) {
	t := time.NewTicker(p.cfg.PollInterval)
	defer t.Stop()
	for {
		select {
		case n := <-w.ch:
			return n, nil
		case <-ctx.Done():
			p.dropWaiter(poolID, w)
			if n := p.drain(w); n != nil { // assigned while we were giving up
				return n, nil
			}
			return nil, fmt.Errorf("no new node appeared in node pool %s within %s", shortID(poolID), p.cfg.ProvisionTimeout)
		case <-t.C:
		}
		np, err := p.getPool(ctx, poolID)
		if err != nil {
			p.dropWaiter(poolID, w)
			if n := p.drain(w); n != nil { // assigned in the meantime: don't leave it behind
				_ = p.Release(context.Background(), n)
			}
			return nil, err
		}
		p.mu.Lock()
		p.observeLocked(poolID, np)
		p.assignLocked(poolID, np)
		p.mu.Unlock()
		if n := p.drain(w); n != nil {
			return n, nil
		}
		report(fmt.Sprintf("waiting for a new node in pool %s (%s)", shortID(poolID), summarize(np)))
	}
}

func (p *OKE) drain(w *waiter) *Node {
	select {
	case n := <-w.ch:
		return n
	default:
		return nil
	}
}

func (p *OKE) dropWaiter(poolID string, w *waiter) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ws := p.waiters[poolID]
	for i, x := range ws {
		if x == w {
			p.waiters[poolID] = append(ws[:i], ws[i+1:]...)
			if !w.accepted && p.unaccepted[poolID] > 0 {
				p.unaccepted[poolID]--
			}
			return
		}
	}
}

// observeLocked records the nodes present the first time a pool is read:
// they predate this controller instance and are never handed to a job.
func (p *OKE) observeLocked(poolID string, np *containerengine.NodePool) {
	if p.seen[poolID] {
		return
	}
	p.seen[poolID] = true
	for _, n := range np.Nodes {
		if n.Id != nil {
			p.foreign[*n.Id] = struct{}{}
		}
	}
}

// assignLocked hands claimable nodes to waiting jobs, oldest waiter first.
func (p *OKE) assignLocked(poolID string, np *containerengine.NodePool) {
	for _, n := range np.Nodes {
		if len(p.waiters[poolID]) == 0 {
			return
		}
		if !p.claimableLocked(n) {
			continue
		}
		w := p.waiters[poolID][0]
		p.waiters[poolID] = p.waiters[poolID][1:]
		node := &Node{ID: *n.Id, PoolID: poolID, JobID: w.jobID}
		p.claimed[node.ID] = node
		w.ch <- node
	}
}

func (p *OKE) claimableLocked(n containerengine.Node) bool {
	if n.Id == nil {
		return false
	}
	switch n.LifecycleState {
	case containerengine.NodeLifecycleStateCreating, containerengine.NodeLifecycleStateUpdating, containerengine.NodeLifecycleStateActive:
	default:
		return false
	}
	if _, ok := p.claimed[*n.Id]; ok {
		return false
	}
	if _, ok := p.doomed[*n.Id]; ok {
		return false
	}
	if _, ok := p.foreign[*n.Id]; ok {
		return false
	}
	return true // a node OKE failed to create is claimed too, so the job fails fast with its error
}

// awaitReady waits for the claimed node to be ACTIVE in OKE and Ready in
// Kubernetes, and returns the Kubernetes node name.
func (p *OKE) awaitReady(ctx context.Context, node *Node, report Reporter) (string, error) {
	t := time.NewTicker(p.cfg.PollInterval)
	defer t.Stop()
	for {
		np, err := p.getPool(ctx, node.PoolID)
		if err != nil {
			return "", err
		}
		var cur *containerengine.Node
		for i := range np.Nodes {
			if np.Nodes[i].Id != nil && *np.Nodes[i].Id == node.ID {
				cur = &np.Nodes[i]
			}
		}
		switch {
		case cur == nil || cur.LifecycleState == containerengine.NodeLifecycleStateDeleted:
			return "", errors.New("node disappeared from the node pool while provisioning")
		case cur.NodeError != nil:
			return "", fmt.Errorf("node pool could not create the node: %s", nodeErrorString(cur.NodeError))
		case cur.LifecycleState == containerengine.NodeLifecycleStateFailing || cur.LifecycleState == containerengine.NodeLifecycleStateInactive ||
			cur.LifecycleState == containerengine.NodeLifecycleStateDeleting:
			return "", fmt.Errorf("node entered state %s while provisioning", cur.LifecycleState)
		case cur.LifecycleState == containerengine.NodeLifecycleStateActive:
			if name, ready := p.kubeNodeReady(ctx, cur); ready {
				return name, nil
			}
			report(fmt.Sprintf("node %s is ACTIVE; waiting for it to join the cluster and become Ready", shortID(node.ID)))
		default:
			report(fmt.Sprintf("node %s is %s", shortID(node.ID), cur.LifecycleState))
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("node %s did not become Ready within %s", shortID(node.ID), p.cfg.ProvisionTimeout)
		case <-t.C:
		}
	}
}

// kubeNodeReady finds the Kubernetes Node for an OKE node (providerID is the
// instance OCID; the node name is normally its private IP) and reports
// whether it is Ready, schedulable and initialised by the cloud controller.
func (p *OKE) kubeNodeReady(ctx context.Context, oke *containerengine.Node) (string, bool) {
	list, err := p.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		p.log.Warn("list nodes failed", "err", err)
		return "", false
	}
	for i := range list.Items {
		n := &list.Items[i]
		match := strings.HasSuffix(n.Spec.ProviderID, *oke.Id) ||
			(oke.PrivateIp != nil && n.Name == *oke.PrivateIp)
		if !match {
			continue
		}
		if n.Spec.Unschedulable {
			return n.Name, false
		}
		for _, t := range n.Spec.Taints {
			if t.Key == "node.cloudprovider.kubernetes.io/uninitialized" {
				return n.Name, false
			}
		}
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
				return n.Name, true
			}
		}
		return n.Name, false
	}
	return "", false
}

// Release deletes the node and decrements the pool size. The pod is already
// gone, so cordon/drain is skipped (PT0M) for the fastest teardown.
func (p *OKE) Release(ctx context.Context, n *Node) error {
	if n == nil || n.ID == "" {
		return nil
	}
	p.mu.Lock()
	p.doomed[n.ID] = struct{}{}
	delete(p.claimed, n.ID)
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, p.cfg.APIRetryWindow+30*time.Second)
	defer cancel()
	err := p.retry(ctx, "DeleteNode", func() error {
		_, err := p.api.DeleteNode(ctx, containerengine.DeleteNodeRequest{
			NodePoolId:                    common.String(n.PoolID),
			NodeId:                        common.String(n.ID),
			IsDecrementSize:               common.Bool(true),
			OverrideEvictionGraceDuration: common.String("PT0M"),
			IsForceDeletionAfterOverrideGraceDuration: common.Bool(true),
			RequestMetadata: requestMetadata(),
		})
		switch {
		case err == nil, isNotFound(err):
			return nil
		case isConflict(err) && p.goneOrDeleting(ctx, n):
			return nil // OKE answers 409 when the node is already being deleted
		}
		return err
	})
	if err != nil {
		return fmt.Errorf("delete node %s: %w", shortID(n.ID), err)
	}
	p.log.Info("node deleted", "job", n.JobID, "node", shortID(n.ID), "pool", shortID(n.PoolID))
	return nil
}

// Sweep deletes every node in the managed pools that is not claimed by an
// active job, after first handing claimable nodes to waiting jobs. On
// start-up nothing is claimed, so it empties the pools.
func (p *OKE) Sweep(ctx context.Context, jobActive func(string) bool) (int, error) {
	if jobActive == nil {
		jobActive = func(string) bool { return false }
	}
	deleted := 0
	var errs []error
	for _, poolID := range p.Pools() {
		np, err := p.getPool(ctx, poolID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var victims []*Node
		p.mu.Lock()
		p.observeLocked(poolID, np)
		p.assignLocked(poolID, np)
		present := map[string]struct{}{}
		for _, n := range np.Nodes {
			if n.Id == nil {
				continue
			}
			present[*n.Id] = struct{}{}
			switch n.LifecycleState {
			case containerengine.NodeLifecycleStateDeleting, containerengine.NodeLifecycleStateDeleted:
				continue
			}
			if owner, ok := p.claimed[*n.Id]; ok {
				if jobActive(owner.JobID) {
					continue
				}
				delete(p.claimed, *n.Id) // job finished without releasing
			}
			victims = append(victims, &Node{ID: *n.Id, PoolID: poolID})
		}
		for id := range p.doomed { // forget nodes that are really gone
			if _, ok := present[id]; !ok {
				delete(p.doomed, id)
			}
		}
		p.mu.Unlock()

		for _, v := range victims {
			p.log.Warn("deleting node no active job owns", "node", shortID(v.ID), "pool", shortID(poolID))
			if err := p.Release(ctx, v); err != nil {
				errs = append(errs, err)
				continue
			}
			deleted++
		}
	}
	return deleted, errors.Join(errs...)
}

func (p *OKE) getPool(ctx context.Context, poolID string) (*containerengine.NodePool, error) {
	var np *containerengine.NodePool
	err := p.retry(ctx, "GetNodePool", func() error {
		resp, err := p.api.GetNodePool(ctx, containerengine.GetNodePoolRequest{
			NodePoolId:      common.String(poolID),
			RequestMetadata: requestMetadata(),
		})
		if err != nil {
			return err
		}
		np = &resp.NodePool
		return nil
	})
	return np, err
}

// retry repeats op on transient OCI errors (HTTP 409, 429, 5xx and transport
// failures) with exponential back-off until the retry window elapses.
func (p *OKE) retry(ctx context.Context, what string, op func() error) error {
	deadline := time.Now().Add(p.cfg.APIRetryWindow)
	delay := p.cfg.retryBase
	for attempt := 1; ; attempt++ {
		err := op()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !isTransient(err) || time.Now().Add(delay).After(deadline) {
			return fmt.Errorf("%s: %w", what, err)
		}
		p.log.Warn("transient OCI error, retrying", "op", what, "attempt", attempt, "in", delay, "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		if delay *= 2; delay > 15*time.Second {
			delay = 15 * time.Second
		}
	}
}

func isTransient(err error) bool {
	if se, ok := common.IsServiceError(err); ok {
		code := se.GetHTTPStatusCode()
		return code == 409 || code == 429 || code >= 500
	}
	return true // transport-level failure
}

func isNotFound(err error) bool {
	se, ok := common.IsServiceError(err)
	return ok && se.GetHTTPStatusCode() == 404
}

func isConflict(err error) bool {
	se, ok := common.IsServiceError(err)
	return ok && se.GetHTTPStatusCode() == 409
}

// goneOrDeleting reports whether the node is absent from its pool or already
// on its way out (DELETING/DELETED), using a single pool read.
func (p *OKE) goneOrDeleting(ctx context.Context, n *Node) bool {
	resp, err := p.api.GetNodePool(ctx, containerengine.GetNodePoolRequest{NodePoolId: common.String(n.PoolID), RequestMetadata: requestMetadata()})
	if err != nil {
		return false
	}
	for _, node := range resp.NodePool.Nodes {
		if node.Id != nil && *node.Id == n.ID {
			return node.LifecycleState == containerengine.NodeLifecycleStateDeleting || node.LifecycleState == containerengine.NodeLifecycleStateDeleted
		}
	}
	return true
}

// requestMetadata enables the SDK's own retry policy (409 IncorrectState,
// 429, 5xx) underneath our coarser retry loop.
func requestMetadata() common.RequestMetadata {
	rp := common.DefaultRetryPolicy()
	return common.RequestMetadata{RetryPolicy: &rp}
}

func desiredSize(np *containerengine.NodePool) int {
	if np.NodeConfigDetails != nil && np.NodeConfigDetails.Size != nil {
		return *np.NodeConfigDetails.Size
	}
	n := 0
	for _, node := range np.Nodes {
		switch node.LifecycleState {
		case containerengine.NodeLifecycleStateDeleting, containerengine.NodeLifecycleStateDeleted:
		default:
			n++
		}
	}
	return n
}

func summarize(np *containerengine.NodePool) string {
	counts := map[containerengine.NodeLifecycleStateEnum]int{}
	for _, n := range np.Nodes {
		counts[n.LifecycleState]++
	}
	parts := make([]string, 0, len(counts))
	for state, c := range counts {
		parts = append(parts, fmt.Sprintf("%d %s", c, strings.ToLower(string(state))))
	}
	sort.Strings(parts)
	if len(parts) == 0 {
		return fmt.Sprintf("size %d, no nodes yet", desiredSize(np))
	}
	return fmt.Sprintf("size %d: %s", desiredSize(np), strings.Join(parts, ", "))
}

func nodeErrorString(e *containerengine.NodeError) string {
	if e == nil {
		return ""
	}
	code, msg := "", ""
	if e.Code != nil {
		code = *e.Code
	}
	if e.Message != nil {
		msg = *e.Message
	}
	switch {
	case code != "" && msg != "":
		return code + ": " + msg
	case code != "":
		return code
	default:
		return msg
	}
}

// shortID trims an OCID to its distinguishing tail for logs and messages.
func shortID(id string) string {
	if len(id) <= 16 {
		return id
	}
	return "…" + id[len(id)-12:]
}
