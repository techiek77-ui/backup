package nodes

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/containerengine"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/techiek77-ui/backup/internal/config"
)

// svcErr imitates an OCI service error.
type svcErr struct {
	code int
	msg  string
}

func (e svcErr) Error() string          { return fmt.Sprintf("http %d: %s", e.code, e.msg) }
func (e svcErr) GetHTTPStatusCode() int { return e.code }
func (e svcErr) GetMessage() string     { return e.msg }
func (e svcErr) GetCode() string        { return "Fake" }
func (e svcErr) GetOpcRequestID() string {
	return "req-1"
}

// fakeOKE models node pools the way OKE behaves for this controller:
// UpdateNodePool(size) adds CREATING nodes, they turn ACTIVE after a few
// reads and then "join" the cluster (a Kubernetes Node appears), DeleteNode
// moves a node to DELETING and shrinks the size when asked.
type fakeOKE struct {
	mu        sync.Mutex
	kube      *fake.Clientset
	pools     map[string]*containerengine.NodePool
	seq       int
	bootPolls int                                   // reads before CREATING -> ACTIVE
	polls     map[string]int                        // node id -> reads since creation
	noJoin    map[string]bool                       // pool -> nodes never join Kubernetes
	noNode    map[string]bool                       // pool -> size grows but no node appears
	failWith  map[string]*containerengine.NodeError // pool -> next created node gets this error
	updateErr int                                   // UpdateNodePool calls to fail (500) first
	updates   int
	deletes   []containerengine.DeleteNodeRequest
}

func newFakeOKE(kube *fake.Clientset, pools ...string) *fakeOKE {
	f := &fakeOKE{kube: kube, pools: map[string]*containerengine.NodePool{}, bootPolls: 2,
		polls: map[string]int{}, noJoin: map[string]bool{}, noNode: map[string]bool{}, failWith: map[string]*containerengine.NodeError{}}
	for _, id := range pools {
		f.pools[id] = &containerengine.NodePool{Id: common.String(id), NodeConfigDetails: &containerengine.NodePoolNodeConfigDetails{Size: common.Int(0)}}
	}
	return f
}

func (f *fakeOKE) addNode(poolID string, state containerengine.NodeLifecycleStateEnum) string {
	f.seq++
	id := fmt.Sprintf("ocid1.instance.oc1.iad.%s-%03d", poolID, f.seq)
	ip := fmt.Sprintf("10.0.0.%d", f.seq)
	f.pools[poolID].Nodes = append(f.pools[poolID].Nodes, containerengine.Node{
		Id: common.String(id), Name: common.String(ip), PrivateIp: common.String(ip), NodePoolId: common.String(poolID), LifecycleState: state,
	})
	return id
}

func (f *fakeOKE) GetNodePool(ctx context.Context, req containerengine.GetNodePoolRequest) (containerengine.GetNodePoolResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	np, ok := f.pools[*req.NodePoolId]
	if !ok {
		return containerengine.GetNodePoolResponse{}, svcErr{404, "NotAuthorizedOrNotFound"}
	}
	kept := np.Nodes[:0]
	for i := range np.Nodes {
		n := np.Nodes[i]
		switch n.LifecycleState {
		case containerengine.NodeLifecycleStateDeleting:
			continue // gone on the next read
		case containerengine.NodeLifecycleStateCreating:
			f.polls[*n.Id]++
			if n.NodeError == nil && f.polls[*n.Id] >= f.bootPolls {
				n.LifecycleState = containerengine.NodeLifecycleStateActive
				if !f.noJoin[*req.NodePoolId] {
					_, err := f.kube.CoreV1().Nodes().Create(ctx, &corev1.Node{
						ObjectMeta: metav1.ObjectMeta{Name: *n.PrivateIp},
						Spec:       corev1.NodeSpec{ProviderID: *n.Id},
						Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
					}, metav1.CreateOptions{})
					if err != nil && !apierrors.IsAlreadyExists(err) {
						panic(err)
					}
				}
			}
		}
		kept = append(kept, n)
	}
	np.Nodes = kept
	cp := *np
	cp.Nodes = append([]containerengine.Node(nil), np.Nodes...)
	size := *np.NodeConfigDetails.Size
	cp.NodeConfigDetails = &containerengine.NodePoolNodeConfigDetails{Size: &size}
	return containerengine.GetNodePoolResponse{NodePool: cp}, nil
}

func (f *fakeOKE) UpdateNodePool(ctx context.Context, req containerengine.UpdateNodePoolRequest) (containerengine.UpdateNodePoolResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates++
	if f.updateErr > 0 {
		f.updateErr--
		return containerengine.UpdateNodePoolResponse{}, svcErr{500, "InternalError"}
	}
	poolID := *req.NodePoolId
	np := f.pools[poolID]
	want := *req.UpdateNodePoolDetails.NodeConfigDetails.Size
	have := 0
	for _, n := range np.Nodes {
		if n.LifecycleState != containerengine.NodeLifecycleStateDeleting {
			have++
		}
	}
	np.NodeConfigDetails.Size = common.Int(want)
	if f.noNode[poolID] {
		return containerengine.UpdateNodePoolResponse{}, nil
	}
	for i := have; i < want; i++ {
		id := f.addNode(poolID, containerengine.NodeLifecycleStateCreating)
		if e := f.failWith[poolID]; e != nil {
			np.Nodes[len(np.Nodes)-1].NodeError = e
			_ = id
		}
	}
	return containerengine.UpdateNodePoolResponse{}, nil
}

func (f *fakeOKE) DeleteNode(ctx context.Context, req containerengine.DeleteNodeRequest) (containerengine.DeleteNodeResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, req)
	np := f.pools[*req.NodePoolId]
	for i := range np.Nodes {
		if *np.Nodes[i].Id == *req.NodeId {
			np.Nodes[i].LifecycleState = containerengine.NodeLifecycleStateDeleting
			if req.IsDecrementSize != nil && *req.IsDecrementSize && *np.NodeConfigDetails.Size > 0 {
				np.NodeConfigDetails.Size = common.Int(*np.NodeConfigDetails.Size - 1)
			}
			return containerengine.DeleteNodeResponse{}, nil
		}
	}
	return containerengine.DeleteNodeResponse{}, svcErr{404, "NotAuthorizedOrNotFound"}
}

func (f *fakeOKE) size(pool string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *f.pools[pool].NodeConfigDetails.Size
}

func (f *fakeOKE) deleted() []containerengine.DeleteNodeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]containerengine.DeleteNodeRequest(nil), f.deletes...)
}

func (f *fakeOKE) nodeIDs(pool string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []string
	for _, n := range f.pools[pool].Nodes {
		ids = append(ids, *n.Id)
	}
	return ids
}

type recorder struct {
	mu   sync.Mutex
	msgs []string
}

func (r *recorder) report(m string) { r.mu.Lock(); r.msgs = append(r.msgs, m); r.mu.Unlock() }
func (r *recorder) all() string     { r.mu.Lock(); defer r.mu.Unlock(); return strings.Join(r.msgs, "\n") }

func newHarness(t *testing.T, cfg OKEConfig) (*fakeOKE, *OKE) {
	t.Helper()
	kube := fake.NewSimpleClientset()
	f := newFakeOKE(kube, "poolA", "poolB")
	cfg.Pools = []string{"poolA", "poolB"}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 10 * time.Millisecond
	}
	if cfg.ProvisionTimeout == 0 {
		cfg.ProvisionTimeout = 3 * time.Second
	}
	if cfg.APIRetryWindow == 0 {
		cfg.APIRetryWindow = 300 * time.Millisecond
	}
	cfg.retryBase = 5 * time.Millisecond
	log := slog.New(slog.NewTextHandler(testWriter{t}, nil))
	return f, NewOKE(f, kube, cfg, log)
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

var classA = config.SizeClass{NodePoolID: "poolA"}

func TestProvisionAndRelease(t *testing.T) {
	f, p := newHarness(t, OKEConfig{})
	rec := &recorder{}
	node, err := p.Provision(context.Background(), "job1", classA, rec.report)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if !strings.HasPrefix(node.ID, "ocid1.instance.") || node.Name != "10.0.0.1" || node.PoolID != "poolA" || node.JobID != "job1" {
		t.Fatalf("node = %+v", node)
	}
	if f.size("poolA") != 1 {
		t.Fatalf("pool size = %d, want 1", f.size("poolA"))
	}
	if !strings.Contains(rec.all(), "waiting") {
		t.Errorf("expected progress messages, got %q", rec.all())
	}

	if err := p.Release(context.Background(), node); err != nil {
		t.Fatalf("release: %v", err)
	}
	d := f.deleted()
	if len(d) != 1 || *d[0].NodeId != node.ID || *d[0].NodePoolId != "poolA" {
		t.Fatalf("deletes = %+v", d)
	}
	if d[0].IsDecrementSize == nil || !*d[0].IsDecrementSize {
		t.Error("DeleteNode must decrement the pool size")
	}
	if d[0].OverrideEvictionGraceDuration == nil || *d[0].OverrideEvictionGraceDuration != "PT0M" ||
		d[0].IsForceDeletionAfterOverrideGraceDuration == nil || !*d[0].IsForceDeletionAfterOverrideGraceDuration {
		t.Error("DeleteNode must skip cordon/drain (PT0M, force) because the pod is already gone")
	}
	if f.size("poolA") != 0 {
		t.Fatalf("pool size after release = %d, want 0", f.size("poolA"))
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.claimed) != 0 || len(p.waiters["poolA"]) != 0 {
		t.Fatalf("bookkeeping not cleared: claimed=%d waiters=%d", len(p.claimed), len(p.waiters["poolA"]))
	}
}

func TestConcurrentJobsGetDistinctNodes(t *testing.T) {
	f, p := newHarness(t, OKEConfig{})
	const n = 4
	var wg sync.WaitGroup
	ids := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			node, err := p.Provision(context.Background(), fmt.Sprintf("job%d", i), classA, nil)
			if err != nil {
				t.Errorf("job%d: %v", i, err)
				return
			}
			ids <- node.ID
		}(i)
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		if seen[id] {
			t.Fatalf("node %s handed to two jobs", id)
		}
		seen[id] = true
	}
	if len(seen) != n || f.size("poolA") != n {
		t.Fatalf("got %d nodes, pool size %d, want %d", len(seen), f.size("poolA"), n)
	}
}

func TestProvisionFailsFastOnNodeError(t *testing.T) {
	f, p := newHarness(t, OKEConfig{ProvisionTimeout: 5 * time.Second})
	f.failWith["poolA"] = &containerengine.NodeError{Code: common.String("InternalError"), Message: common.String("Out of host capacity.")}
	start := time.Now()
	_, err := p.Provision(context.Background(), "job1", classA, nil)
	if err == nil || !strings.Contains(err.Error(), "Out of host capacity") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("should fail fast, took %s", time.Since(start))
	}
	d := f.deleted()
	if len(d) != 1 || d[0].IsDecrementSize == nil || !*d[0].IsDecrementSize {
		t.Fatalf("failed node must be deleted with decrement, deletes=%+v", d)
	}
	if f.size("poolA") != 0 {
		t.Fatalf("pool size = %d, want 0", f.size("poolA"))
	}
}

func TestProvisionTimesOutWhenNoNodeAppears(t *testing.T) {
	f, p := newHarness(t, OKEConfig{ProvisionTimeout: 150 * time.Millisecond})
	f.noNode["poolA"] = true
	_, err := p.Provision(context.Background(), "job1", classA, nil)
	if err == nil || !strings.Contains(err.Error(), "no new node appeared") {
		t.Fatalf("err = %v", err)
	}
	if len(f.deleted()) != 0 {
		t.Fatalf("nothing was claimed, nothing should be deleted: %+v", f.deleted())
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.waiters["poolA"]) != 0 {
		t.Fatal("timed-out waiter must be removed from the queue")
	}
}

func TestProvisionTimesOutWhenNodeNeverJoins(t *testing.T) {
	f, p := newHarness(t, OKEConfig{ProvisionTimeout: 300 * time.Millisecond})
	f.noJoin["poolA"] = true
	rec := &recorder{}
	_, err := p.Provision(context.Background(), "job1", classA, rec.report)
	if err == nil || !strings.Contains(err.Error(), "did not become Ready") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(rec.all(), "ACTIVE; waiting for it to join") {
		t.Errorf("expected join-wait progress, got %q", rec.all())
	}
	if d := f.deleted(); len(d) != 1 {
		t.Fatalf("claimed node must be torn down after a failed provision, deletes=%+v", d)
	}
}

func TestProvisionRetriesTransientUpdateErrors(t *testing.T) {
	f, p := newHarness(t, OKEConfig{})
	f.updateErr = 2
	if _, err := p.Provision(context.Background(), "job1", classA, nil); err != nil {
		t.Fatalf("provision: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updates != 3 {
		t.Fatalf("UpdateNodePool calls = %d, want 3", f.updates)
	}
}

func TestSweepEmptiesPoolsOnStartupAndNeverReusesOldNodes(t *testing.T) {
	f, p := newHarness(t, OKEConfig{})
	f.mu.Lock()
	old := f.addNode("poolA", containerengine.NodeLifecycleStateActive)
	f.pools["poolA"].NodeConfigDetails.Size = common.Int(1)
	f.mu.Unlock()

	n, err := p.Sweep(context.Background(), nil)
	if err != nil || n != 1 {
		t.Fatalf("sweep n=%d err=%v", n, err)
	}
	if d := f.deleted(); len(d) != 1 || *d[0].NodeId != old {
		t.Fatalf("deletes = %+v", d)
	}
	p.mu.Lock()
	_, foreign := p.foreign[old]
	p.mu.Unlock()
	if !foreign {
		t.Fatal("pre-existing node must be recorded as foreign")
	}

	node, err := p.Provision(context.Background(), "job1", classA, nil)
	if err != nil {
		t.Fatal(err)
	}
	if node.ID == old {
		t.Fatal("a pre-existing node must never be handed to a job")
	}
}

func TestSweepKeepsActiveJobNodeRemovesFinishedOne(t *testing.T) {
	f, p := newHarness(t, OKEConfig{})
	n1, err := p.Provision(context.Background(), "job1", classA, nil)
	if err != nil {
		t.Fatal(err)
	}
	n2, err := p.Provision(context.Background(), "job2", classA, nil)
	if err != nil {
		t.Fatal(err)
	}
	active := map[string]bool{"job1": true}
	n, err := p.Sweep(context.Background(), func(id string) bool { return active[id] })
	if err != nil || n != 1 {
		t.Fatalf("sweep n=%d err=%v", n, err)
	}
	d := f.deleted()
	if len(d) != 1 || *d[0].NodeId != n2.ID {
		t.Fatalf("expected only job2's node deleted, got %+v", d)
	}
	ids := f.nodeIDs("poolA")
	if len(ids) == 0 || ids[0] != n1.ID {
		t.Fatalf("job1's node must survive, pool now has %v", ids)
	}
}

func TestSweepHandsNewNodeToWaiterInsteadOfDeleting(t *testing.T) {
	f, p := newHarness(t, OKEConfig{})
	f.bootPolls = 1000 // node stays CREATING for the whole test
	done := make(chan error, 1)
	go func() {
		_, err := p.Provision(context.Background(), "job1", classA, nil)
		done <- err
	}()
	// wait until the scale-up produced a CREATING node
	deadline := time.Now().Add(2 * time.Second)
	for len(f.nodeIDs("poolA")) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("scale-up never produced a node")
		}
		time.Sleep(5 * time.Millisecond)
	}
	n, err := p.Sweep(context.Background(), func(string) bool { return true })
	if err != nil || n != 0 {
		t.Fatalf("sweep must not delete a node a waiting job is about to claim: n=%d err=%v", n, err)
	}
	f.mu.Lock()
	f.bootPolls = 1 // let it boot now
	f.mu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("provision after sweep: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("provision did not finish")
	}
}
