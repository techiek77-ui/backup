// Package spawner creates a worker Pod for a job, waits for it to finish,
// collects its logs and exit code, and deletes it.
package spawner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"

	"github.com/techiek77-ui/backup/internal/store"
)

const (
	LabelApp   = "app.kubernetes.io/name"
	LabelJobID = "podspawner.io/job-id"
	AppName    = "podspawner-worker"
	// ContainerName is the single container in every worker pod.
	ContainerName = "worker"
)

// Config controls how worker pods are built and supervised.
type Config struct {
	Namespace string
	// NodeSelector pins workers to labelled nodes, e.g. {"workload": "worker"}.
	// On OKE this is typically a label applied to a node pool.
	NodeSelector map[string]string
	// Tolerations let workers land on a tainted (dedicated) node pool.
	Tolerations []corev1.Toleration
	// ImagePullSecrets for the worker image (e.g. an OCIR pull secret).
	ImagePullSecrets []string
	// ServiceAccount for the worker pod; empty means the namespace default.
	ServiceAccount string
	// DefaultTimeout bounds pod runtime when the request doesn't specify one.
	DefaultTimeout time.Duration
	// ScheduleTimeout is how long a pod may stay unscheduled before we give
	// up. Pods that never get a node would otherwise hang forever, since
	// activeDeadlineSeconds only counts once a kubelet has picked them up.
	ScheduleTimeout time.Duration
	// MaxLogBytes caps how much log we keep per job.
	MaxLogBytes int64
	// KeepFailedPods leaves failed pods around for debugging instead of deleting.
	KeepFailedPods bool
}

type Spawner struct {
	client kubernetes.Interface
	cfg    Config
	store  *store.Store
	log    *slog.Logger
}

func New(client kubernetes.Interface, cfg Config, st *store.Store, log *slog.Logger) *Spawner {
	if cfg.DefaultTimeout <= 0 {
		cfg.DefaultTimeout = 10 * time.Minute
	}
	if cfg.ScheduleTimeout <= 0 {
		cfg.ScheduleTimeout = 2 * time.Minute
	}
	if cfg.MaxLogBytes <= 0 {
		cfg.MaxLogBytes = 64 * 1024
	}
	if log == nil {
		log = slog.Default()
	}
	return &Spawner{client: client, cfg: cfg, store: st, log: log}
}

func (s *Spawner) timeoutFor(job *store.Job) time.Duration {
	if job.Spec.TimeoutSeconds > 0 {
		return time.Duration(job.Spec.TimeoutSeconds) * time.Second
	}
	return s.cfg.DefaultTimeout
}

// BuildPod returns the Pod manifest for a job. Exposed for tests and for
// users who want to see exactly what gets submitted.
func (s *Spawner) BuildPod(job *store.Job) *corev1.Pod {
	deadline := int64(s.timeoutFor(job).Seconds())
	automount := false

	var pullSecrets []corev1.LocalObjectReference
	for _, name := range s.cfg.ImagePullSecrets {
		pullSecrets = append(pullSecrets, corev1.LocalObjectReference{Name: name})
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			// Job IDs are lowercase hex, so this is a valid DNS-1123 prefix.
			GenerateName: "work-" + job.ID + "-",
			Namespace:    s.cfg.Namespace,
			Labels: map[string]string{
				LabelApp:   AppName,
				LabelJobID: job.ID,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			ActiveDeadlineSeconds:        &deadline,
			NodeSelector:                 s.cfg.NodeSelector,
			Tolerations:                  s.cfg.Tolerations,
			ImagePullSecrets:             pullSecrets,
			ServiceAccountName:           s.cfg.ServiceAccount,
			AutomountServiceAccountToken: &automount,
			Containers: []corev1.Container{{
				Name:    ContainerName,
				Image:   job.Spec.Image,
				Command: job.Spec.Command,
				Args:    job.Spec.Args,
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("50m"),
						corev1.ResourceMemory: resource.MustParse("64Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
				},
			}},
		},
	}
}

// Run drives one job to completion. It blocks until the pod finishes (or
// the context is cancelled) and always deletes the pod afterwards, except
// for failed pods when KeepFailedPods is set. Intended to run in its own
// goroutine per job.
func (s *Spawner) Run(ctx context.Context, jobID string) {
	job, ok := s.store.Get(jobID)
	if !ok {
		s.log.Error("job not found", "job", jobID)
		return
	}
	log := s.log.With("job", jobID)

	pods := s.client.CoreV1().Pods(s.cfg.Namespace)
	created, err := pods.Create(ctx, s.BuildPod(job), metav1.CreateOptions{})
	if err != nil {
		s.fail(jobID, "", fmt.Errorf("create pod: %w", err))
		return
	}
	log = log.With("pod", created.Name)
	log.Info("worker pod created")

	now := time.Now()
	s.store.Update(jobID, func(j *store.Job) {
		j.Status = store.StatusRunning
		j.PodName = created.Name
		j.StartedAt = &now
	})

	keep := false
	defer func() {
		if !keep {
			s.deletePod(created.Name, log)
		}
	}()

	// Backstop beyond activeDeadlineSeconds, in case the kubelet never
	// enforces it (e.g. node lost).
	waitTimeout := s.timeoutFor(job) + 30*time.Second
	waitCtx, cancel := context.WithTimeout(ctx, waitTimeout)
	defer cancel()

	final, err := s.waitForCompletion(waitCtx, jobID, created.Name, created.ResourceVersion)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			err = fmt.Errorf("timed out after %s waiting for pod to finish", waitTimeout)
		}
		s.fail(jobID, created.Name, err)
		return
	}

	logs := s.fetchLogs(ctx, created.Name, log)
	exit := containerExitCode(final)

	status := store.StatusSucceeded
	if final.Status.Phase != corev1.PodSucceeded {
		status = store.StatusFailed
	}
	ended := time.Now()
	s.store.Update(jobID, func(j *store.Job) {
		j.Status = status
		j.NodeName = final.Spec.NodeName
		j.ExitCode = exit
		j.Logs = logs
		j.Message = ""
		j.EndedAt = &ended
		if status == store.StatusFailed {
			j.Error = podFailureReason(final)
		}
	})
	log.Info("worker pod finished", "phase", final.Status.Phase, "node", final.Spec.NodeName)

	if status == store.StatusFailed && s.cfg.KeepFailedPods {
		log.Warn("keeping failed pod for inspection (KEEP_FAILED_PODS=true)")
		keep = true
	}
}

// waitForCompletion watches a single pod until it reaches a terminal phase.
func (s *Spawner) waitForCompletion(ctx context.Context, jobID, name, rv string) (*corev1.Pod, error) {
	pods := s.client.CoreV1().Pods(s.cfg.Namespace)

	// Check current state first in case the pod already finished before the
	// watch is established (very short commands).
	scheduled := false
	if cur, err := pods.Get(ctx, name, metav1.GetOptions{}); err == nil {
		if isTerminal(cur) {
			return cur, nil
		}
		scheduled = cur.Spec.NodeName != ""
		s.observe(jobID, cur)
	}

	w, err := pods.Watch(ctx, metav1.ListOptions{
		FieldSelector:   fields.OneTermEqualSelector("metadata.name", name).String(),
		ResourceVersion: rv,
	})
	if err != nil {
		return nil, fmt.Errorf("watch pod: %w", err)
	}
	defer w.Stop()

	schedTimer := time.NewTimer(s.cfg.ScheduleTimeout)
	defer schedTimer.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()

		case <-schedTimer.C:
			if scheduled {
				continue
			}
			msg := fmt.Sprintf("pod was not scheduled within %s", s.cfg.ScheduleTimeout)
			if j, ok := s.store.Get(jobID); ok && j.Message != "" {
				msg += ": " + j.Message
			}
			return nil, errors.New(msg)

		case ev, ok := <-w.ResultChan():
			if !ok {
				// Watch closed by the API server; re-check state once.
				cur, err := pods.Get(ctx, name, metav1.GetOptions{})
				if err != nil {
					return nil, fmt.Errorf("watch closed and get failed: %w", err)
				}
				if isTerminal(cur) {
					return cur, nil
				}
				return nil, fmt.Errorf("watch closed before pod finished (phase %s)", cur.Status.Phase)
			}
			switch ev.Type {
			case watch.Deleted:
				return nil, errors.New("pod was deleted externally")
			case watch.Error:
				return nil, fmt.Errorf("watch error: %v", ev.Object)
			}
			p, ok := ev.Object.(*corev1.Pod)
			if !ok || p.Name != name {
				continue
			}
			if p.Spec.NodeName != "" {
				scheduled = true
			}
			s.observe(jobID, p)
			if isTerminal(p) {
				return p, nil
			}
		}
	}
}

// observe copies useful in-flight facts (node, scheduling or image-pull
// problems) onto the job so GET /work/{id} explains what is going on.
func (s *Spawner) observe(jobID string, p *corev1.Pod) {
	msg := ""
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
			msg = c.Reason
			if c.Message != "" {
				msg += ": " + c.Message
			}
		}
	}
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Name == ContainerName && cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			msg = cs.State.Waiting.Reason
			if cs.State.Waiting.Message != "" {
				msg += ": " + cs.State.Waiting.Message
			}
		}
	}
	s.store.Update(jobID, func(j *store.Job) {
		if p.Spec.NodeName != "" {
			j.NodeName = p.Spec.NodeName
		}
		j.Message = msg
	})
}

func (s *Spawner) fetchLogs(ctx context.Context, name string, log *slog.Logger) string {
	limit := s.cfg.MaxLogBytes
	req := s.client.CoreV1().Pods(s.cfg.Namespace).GetLogs(name, &corev1.PodLogOptions{
		Container:  ContainerName,
		LimitBytes: &limit,
	})
	rc, err := req.Stream(ctx)
	if err != nil {
		log.Warn("could not fetch logs", "err", err)
		return ""
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, limit))
	if err != nil {
		log.Warn("error reading logs", "err", err)
	}
	return string(b)
}

func (s *Spawner) deletePod(name string, log *slog.Logger) {
	// Cleanup must not depend on the (possibly cancelled) job context.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	policy := metav1.DeletePropagationBackground
	err := s.client.CoreV1().Pods(s.cfg.Namespace).Delete(ctx, name, metav1.DeleteOptions{
		PropagationPolicy: &policy,
	})
	if err != nil {
		log.Error("failed to delete worker pod", "err", err)
		return
	}
	log.Info("worker pod deleted")
}

// CleanupOrphans deletes every worker pod carrying our label. It is called on
// controller start-up: job state is in-memory, so pods left behind by a
// previous instance can never be collected and would otherwise leak.
func (s *Spawner) CleanupOrphans(ctx context.Context) (int, error) {
	pods := s.client.CoreV1().Pods(s.cfg.Namespace)
	list, err := pods.List(ctx, metav1.ListOptions{LabelSelector: LabelApp + "=" + AppName})
	if err != nil {
		return 0, fmt.Errorf("list worker pods: %w", err)
	}
	n := 0
	for i := range list.Items {
		p := &list.Items[i]
		if err := pods.Delete(ctx, p.Name, metav1.DeleteOptions{}); err != nil {
			s.log.Warn("failed to delete orphan pod", "pod", p.Name, "err", err)
			continue
		}
		s.log.Info("deleted orphan worker pod", "pod", p.Name, "phase", p.Status.Phase)
		n++
	}
	return n, nil
}

func (s *Spawner) fail(jobID, podName string, err error) {
	s.log.Error("job failed", "job", jobID, "pod", podName, "err", err)
	ended := time.Now()
	s.store.Update(jobID, func(j *store.Job) {
		j.Status = store.StatusFailed
		j.Error = err.Error()
		j.EndedAt = &ended
		if podName != "" {
			j.PodName = podName
		}
	})
}

func isTerminal(p *corev1.Pod) bool {
	return p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed
}

func containerExitCode(p *corev1.Pod) *int32 {
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Name == ContainerName && cs.State.Terminated != nil {
			code := cs.State.Terminated.ExitCode
			return &code
		}
	}
	return nil
}

func podFailureReason(p *corev1.Pod) string {
	if p.Status.Reason != "" { // e.g. DeadlineExceeded, Evicted
		if p.Status.Message != "" {
			return p.Status.Reason + ": " + p.Status.Message
		}
		return p.Status.Reason
	}
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Name == ContainerName && cs.State.Terminated != nil {
			t := cs.State.Terminated
			if t.Reason != "" && t.Reason != "Error" {
				return fmt.Sprintf("%s (exit code %d)", t.Reason, t.ExitCode)
			}
			return fmt.Sprintf("container exited with code %d", t.ExitCode)
		}
	}
	return "pod failed"
}
