package spawner

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/techiek77-ui/backup/internal/store"
)

const testNS = "podspawner"

type harness struct {
	client     *fake.Clientset
	store      *store.Store
	sp         *Spawner
	watchReady chan struct{}
}

func newHarness(t *testing.T, cfg Config) *harness {
	t.Helper()
	client := fake.NewSimpleClientset()

	// The fake tracker does not resolve GenerateName; emulate the API server.
	client.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pod := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		if pod.Name == "" && pod.GenerateName != "" {
			pod.Name = pod.GenerateName + "abcde"
		}
		return false, nil, nil
	})

	// Signal when the spawner has established its watch, so tests can mutate
	// the pod only after events will be delivered (the fake ignores RV).
	watchReady := make(chan struct{}, 8)
	gvr := corev1.SchemeGroupVersion.WithResource("pods")
	client.PrependWatchReactor("pods", func(action k8stesting.Action) (bool, watch.Interface, error) {
		w, err := client.Tracker().Watch(gvr, action.GetNamespace())
		if err != nil {
			return false, nil, err
		}
		watchReady <- struct{}{}
		return true, w, nil
	})

	cfg.Namespace = testNS
	st := store.New()
	sp := New(client, cfg, st, slog.New(slog.NewTextHandler(testWriter{t}, nil)))
	return &harness{client: client, store: st, sp: sp, watchReady: watchReady}
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func (h *harness) submit(t *testing.T, id string, spec store.WorkSpec) <-chan struct{} {
	t.Helper()
	h.store.Put(&store.Job{ID: id, Spec: spec, Status: store.StatusPending, CreatedAt: time.Now()})
	done := make(chan struct{})
	go func() { h.sp.Run(context.Background(), id); close(done) }()
	return done
}

func (h *harness) awaitWatch(t *testing.T) {
	t.Helper()
	select {
	case <-h.watchReady:
	case <-time.After(5 * time.Second):
		t.Fatal("spawner never established a watch")
	}
}

func (h *harness) workerPod(t *testing.T, id string) *corev1.Pod {
	t.Helper()
	list, err := h.client.CoreV1().Pods(testNS).List(context.Background(), metav1.ListOptions{LabelSelector: LabelJobID + "=" + id})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("want exactly 1 worker pod, got %d", len(list.Items))
	}
	return &list.Items[0]
}

func (h *harness) finishPod(t *testing.T, pod *corev1.Pod, phase corev1.PodPhase, exit int32) {
	t.Helper()
	pod.Spec.NodeName = "worker-1"
	pod.Status.Phase = phase
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  ContainerName,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exit}},
	}}
	if _, err := h.client.CoreV1().Pods(testNS).Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func waitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
}

func (h *harness) assertPodGone(t *testing.T, name string) {
	t.Helper()
	_, err := h.client.CoreV1().Pods(testNS).Get(context.Background(), name, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected pod %s to be deleted, got err=%v", name, err)
	}
}

func TestBuildPod(t *testing.T) {
	h := newHarness(t, Config{
		NodeSelector:     map[string]string{"workload": "worker"},
		Tolerations:      []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "worker", Effect: corev1.TaintEffectNoSchedule}},
		ImagePullSecrets: []string{"ocirsecret"},
		ServiceAccount:   "podspawner-worker",
		DefaultTimeout:   5 * time.Minute,
	})
	job := &store.Job{ID: "abc123", Spec: store.WorkSpec{Image: "busybox", Command: []string{"sh", "-c", "true"}, TimeoutSeconds: 42}}
	pod := h.sp.BuildPod(job)

	if pod.GenerateName != "work-abc123-" {
		t.Errorf("generateName = %q", pod.GenerateName)
	}
	if pod.Labels[LabelJobID] != "abc123" || pod.Labels[LabelApp] != AppName {
		t.Errorf("labels = %v", pod.Labels)
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %v", pod.Spec.RestartPolicy)
	}
	if got := *pod.Spec.ActiveDeadlineSeconds; got != 42 {
		t.Errorf("activeDeadlineSeconds = %d, want request override 42", got)
	}
	if pod.Spec.NodeSelector["workload"] != "worker" {
		t.Errorf("nodeSelector = %v", pod.Spec.NodeSelector)
	}
	if len(pod.Spec.Tolerations) != 1 || pod.Spec.Tolerations[0].Key != "dedicated" {
		t.Errorf("tolerations = %v", pod.Spec.Tolerations)
	}
	if len(pod.Spec.ImagePullSecrets) != 1 || pod.Spec.ImagePullSecrets[0].Name != "ocirsecret" {
		t.Errorf("imagePullSecrets = %v", pod.Spec.ImagePullSecrets)
	}
	if pod.Spec.ServiceAccountName != "podspawner-worker" || *pod.Spec.AutomountServiceAccountToken {
		t.Errorf("service account settings wrong: %q automount=%v", pod.Spec.ServiceAccountName, *pod.Spec.AutomountServiceAccountToken)
	}
	if c := pod.Spec.Containers[0]; c.Name != ContainerName || c.Image != "busybox" || len(c.Command) != 3 {
		t.Errorf("container = %+v", c)
	}

	// Default timeout applies when the request does not set one.
	job.Spec.TimeoutSeconds = 0
	if got := *h.sp.BuildPod(job).Spec.ActiveDeadlineSeconds; got != 300 {
		t.Errorf("default activeDeadlineSeconds = %d, want 300", got)
	}
}

func TestRunSucceeded(t *testing.T) {
	h := newHarness(t, Config{NodeSelector: map[string]string{"workload": "worker"}})
	done := h.submit(t, "job1", store.WorkSpec{Image: "busybox", Command: []string{"true"}})
	h.awaitWatch(t)

	pod := h.workerPod(t, "job1")
	if j, _ := h.store.Get("job1"); j.Status != store.StatusRunning || j.PodName != pod.Name {
		t.Fatalf("job should be Running with pod %s, got %+v", pod.Name, j)
	}
	h.finishPod(t, pod, corev1.PodSucceeded, 0)
	waitDone(t, done)

	j, _ := h.store.Get("job1")
	if j.Status != store.StatusSucceeded {
		t.Fatalf("status = %s (err=%q)", j.Status, j.Error)
	}
	if j.ExitCode == nil || *j.ExitCode != 0 {
		t.Errorf("exitCode = %v", j.ExitCode)
	}
	if j.NodeName != "worker-1" {
		t.Errorf("nodeName = %q", j.NodeName)
	}
	if j.Logs != "fake logs" { // what client-go's fake GetLogs returns
		t.Errorf("logs = %q", j.Logs)
	}
	if j.EndedAt == nil || j.StartedAt == nil {
		t.Errorf("timestamps missing: %+v", j)
	}
	h.assertPodGone(t, pod.Name)
}

func TestRunFailedExitCode(t *testing.T) {
	h := newHarness(t, Config{})
	done := h.submit(t, "job2", store.WorkSpec{Image: "busybox", Command: []string{"false"}})
	h.awaitWatch(t)

	pod := h.workerPod(t, "job2")
	h.finishPod(t, pod, corev1.PodFailed, 2)
	waitDone(t, done)

	j, _ := h.store.Get("job2")
	if j.Status != store.StatusFailed {
		t.Fatalf("status = %s", j.Status)
	}
	if j.ExitCode == nil || *j.ExitCode != 2 {
		t.Errorf("exitCode = %v", j.ExitCode)
	}
	if !strings.Contains(j.Error, "exited with code 2") {
		t.Errorf("error = %q", j.Error)
	}
	if j.Logs != "fake logs" {
		t.Errorf("logs should still be collected for failed pods, got %q", j.Logs)
	}
	h.assertPodGone(t, pod.Name)
}

func TestRunKeepFailedPods(t *testing.T) {
	h := newHarness(t, Config{KeepFailedPods: true})
	done := h.submit(t, "job3", store.WorkSpec{Image: "busybox"})
	h.awaitWatch(t)

	pod := h.workerPod(t, "job3")
	h.finishPod(t, pod, corev1.PodFailed, 1)
	waitDone(t, done)

	if _, err := h.client.CoreV1().Pods(testNS).Get(context.Background(), pod.Name, metav1.GetOptions{}); err != nil {
		t.Fatalf("failed pod should have been kept, got %v", err)
	}
}

func TestRunUnschedulableTimesOut(t *testing.T) {
	h := newHarness(t, Config{ScheduleTimeout: 100 * time.Millisecond, NodeSelector: map[string]string{"workload": "worker"}})
	done := h.submit(t, "job4", store.WorkSpec{Image: "busybox"})
	h.awaitWatch(t)

	// Simulate the scheduler reporting no matching node; pod never starts.
	pod := h.workerPod(t, "job4")
	pod.Status.Conditions = []corev1.PodCondition{{
		Type:    corev1.PodScheduled,
		Status:  corev1.ConditionFalse,
		Reason:  "Unschedulable",
		Message: "0/3 nodes are available: 3 node(s) didn't match Pod's node affinity/selector.",
	}}
	if _, err := h.client.CoreV1().Pods(testNS).Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitDone(t, done)

	j, _ := h.store.Get("job4")
	if j.Status != store.StatusFailed {
		t.Fatalf("status = %s", j.Status)
	}
	if !strings.Contains(j.Error, "not scheduled within") || !strings.Contains(j.Error, "didn't match Pod's node affinity/selector") {
		t.Errorf("error should explain scheduling failure, got %q", j.Error)
	}
	h.assertPodGone(t, pod.Name)
}

func TestRunPodDeletedExternally(t *testing.T) {
	h := newHarness(t, Config{})
	done := h.submit(t, "job5", store.WorkSpec{Image: "busybox"})
	h.awaitWatch(t)

	pod := h.workerPod(t, "job5")
	if err := h.client.CoreV1().Pods(testNS).Delete(context.Background(), pod.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	waitDone(t, done)

	j, _ := h.store.Get("job5")
	if j.Status != store.StatusFailed || !strings.Contains(j.Error, "deleted externally") {
		t.Fatalf("got status=%s err=%q", j.Status, j.Error)
	}
}

func TestRunContextCancelledDeletesPod(t *testing.T) {
	h := newHarness(t, Config{})
	h.store.Put(&store.Job{ID: "job6", Spec: store.WorkSpec{Image: "busybox"}, Status: store.StatusPending, CreatedAt: time.Now()})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.sp.Run(ctx, "job6"); close(done) }()
	h.awaitWatch(t)

	pod := h.workerPod(t, "job6")
	cancel() // controller shutting down
	waitDone(t, done)

	j, _ := h.store.Get("job6")
	if j.Status != store.StatusFailed {
		t.Fatalf("status = %s", j.Status)
	}
	h.assertPodGone(t, pod.Name)
}

func TestCleanupOrphans(t *testing.T) {
	h := newHarness(t, Config{})
	ctx := context.Background()
	mk := func(name string, labels map[string]string) {
		_, err := h.client.CoreV1().Pods(testNS).Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
	}
	mk("work-old-1", map[string]string{LabelApp: AppName, LabelJobID: "old1"})
	mk("work-old-2", map[string]string{LabelApp: AppName, LabelJobID: "old2"})
	mk("unrelated", map[string]string{"app": "something-else"})

	n, err := h.sp.CleanupOrphans(ctx)
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	h.assertPodGone(t, "work-old-1")
	h.assertPodGone(t, "work-old-2")
	if _, err := h.client.CoreV1().Pods(testNS).Get(ctx, "unrelated", metav1.GetOptions{}); err != nil {
		t.Fatalf("unrelated pod must survive: %v", err)
	}
}
