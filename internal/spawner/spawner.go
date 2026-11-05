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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	// ImagePullTimeout fails a job whose image has been unpullable
	// (ErrImagePull / ImagePullBackOff) for this long instead of waiting for
	// activeDeadlineSeconds. Permanent pull errors fail immediately.
	ImagePullTimeout time.Duration
	// APIRetryWindow is how long transient API-server errors (5xx, timeouts,
	// connection failures) are retried before a job is failed.
	APIRetryWindow time.Duration
	// MaxLogBytes caps how much log we keep per job.
	MaxLogBytes int64
	// KeepFailedPods leaves failed pods around for debugging instead of deleting.
	KeepFailedPods bool

	// Tunables for tests; zero means the production default.
	retryBase    time.Duration // first retry backoff (1s)
	deleteWait   time.Duration // wait for a graceful delete to complete (45s)
	pollInterval time.Duration // delete confirmation poll interval (2s)
}

type Spawner struct {
	client kubernetes.Interface
	cfg    Config
	store  *store.Store
	log    *slog.Logger
}

func New(client kubernetes.Interface, cfg Config, st *store.Store, log *slog.Logger) *Spawner {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&cfg.DefaultTimeout, 10*time.Minute)
	def(&cfg.ScheduleTimeout, 2*time.Minute)
	def(&cfg.ImagePullTimeout, 3*time.Minute)
	def(&cfg.APIRetryWindow, 2*time.Minute)
	def(&cfg.retryBase, time.Second)
	def(&cfg.deleteWait, 45*time.Second)
	def(&cfg.pollInterval, 2*time.Second)
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

	created, err := s.createPod(ctx, job, log)
	if err != nil {
		s.fail(jobID, "", err)
		return
	}
	log = log.With("pod", created.Name)

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

	final, err := s.waitForCompletion(waitCtx, jobID, created.Name)
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

// createPod submits the worker pod, retrying transient API errors. A retry
// first looks for a pod already labelled with this job ID — the previous
// request may have succeeded while its response was lost — and adopts it
// rather than creating a duplicate.
func (s *Spawner) createPod(ctx context.Context, job *store.Job, log *slog.Logger) (*corev1.Pod, error) {
	pods := s.client.CoreV1().Pods(s.cfg.Namespace)
	var created *corev1.Pod
	attempt := 0
	err := s.retry(ctx, "create pod", func() error {
		attempt++
		if attempt > 1 {
			if existing, err := s.findPodForJob(ctx, job.ID); err == nil && existing != nil {
				log.Warn("adopting worker pod created by an earlier attempt", "pod", existing.Name)
				created = existing
				return nil
			}
		}
		p, err := pods.Create(ctx, s.BuildPod(job), metav1.CreateOptions{})
		if err != nil {
			return err
		}
		created = p
		log.Info("worker pod created", "pod", p.Name)
		return nil
	})
	return created, err
}

func (s *Spawner) findPodForJob(ctx context.Context, jobID string) (*corev1.Pod, error) {
	list, err := s.client.CoreV1().Pods(s.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: LabelJobID + "=" + jobID,
	})
	if err != nil {
		return nil, err
	}
	if len(list.Items) == 0 {
		return nil, nil
	}
	return &list.Items[0], nil
}

// podState is what the watcher has learned about the pod so far.
type podState struct {
	scheduled        bool
	message          string    // latest scheduling / waiting reason
	pullReason       string    // non-empty while the image cannot be pulled
	pullFailingSince time.Time // when pullReason first appeared
}

// imagePullFailure returns a job error once the image has been unpullable
// for longer than timeout, or immediately for permanent failures.
func (st *podState) imagePullFailure(timeout time.Duration) string {
	switch st.pullReason {
	case "":
		return ""
	case "InvalidImageName", "ErrImageNeverPull":
		return "image cannot be pulled: " + st.message
	}
	if time.Since(st.pullFailingSince) < timeout {
		return ""
	}
	return fmt.Sprintf("image could not be pulled within %s: %s", timeout, st.message)
}

// waitForCompletion follows a single pod until it reaches a terminal phase.
//
// The API server closes long-lived watches periodically and answers watches
// that resume from a stale resource version with 410 Gone, so a closed watch
// is never treated as a job failure: the pod is re-read with a GET (which
// also catches pods that finished before the watch was established) and a
// new watch is started from the GET's resource version, leaving no gap.
// Transient API errors on the GET and the watch are retried.
func (s *Spawner) waitForCompletion(ctx context.Context, jobID, name string) (*corev1.Pod, error) {
	pods := s.client.CoreV1().Pods(s.cfg.Namespace)

	schedTimer := time.NewTimer(s.cfg.ScheduleTimeout)
	defer schedTimer.Stop()
	st := &podState{}

	for attempt := 0; ; attempt++ {
		var cur *corev1.Pod
		err := s.retry(ctx, "get pod", func() error {
			p, err := pods.Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			cur = p
			return nil
		})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return nil, errors.New("pod was deleted externally")
			}
			return nil, err
		}
		if isTerminal(cur) {
			return cur, nil
		}
		s.observe(jobID, cur, st)
		if msg := st.imagePullFailure(s.cfg.ImagePullTimeout); msg != "" {
			return nil, errors.New(msg)
		}

		var w watch.Interface
		err = s.retry(ctx, "watch pod", func() error {
			ww, err := pods.Watch(ctx, metav1.ListOptions{
				FieldSelector:   fields.OneTermEqualSelector("metadata.name", name).String(),
				ResourceVersion: cur.ResourceVersion,
			})
			if err != nil {
				return err
			}
			w = ww
			return nil
		})
		if err != nil {
			return nil, err
		}
		final, retry, err := s.consumeWatch(ctx, w, jobID, name, st, schedTimer)
		w.Stop()
		if err != nil || final != nil {
			return final, err
		}
		if !retry {
			return nil, errors.New("watch ended unexpectedly")
		}
		s.log.Debug("watch closed, re-establishing", "job", jobID, "pod", name, "attempt", attempt+1)
		// Brief pause so a misbehaving proxy cannot turn this into a hot loop.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// consumeWatch drains events until the pod is terminal (returned), the watch
// is closed or expired (retry=true), or something fatal happens (err).
func (s *Spawner) consumeWatch(ctx context.Context, w watch.Interface, jobID, name string, st *podState, schedTimer *time.Timer) (*corev1.Pod, bool, error) {
	// Image-pull back-off produces status updates only sporadically, so the
	// pull deadline is also checked on a ticker.
	pullCheck := s.cfg.ImagePullTimeout / 4
	if pullCheck < 50*time.Millisecond {
		pullCheck = 50 * time.Millisecond
	}
	if pullCheck > 15*time.Second {
		pullCheck = 15 * time.Second
	}
	ticker := time.NewTicker(pullCheck)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()

		case <-schedTimer.C:
			if st.scheduled {
				continue
			}
			msg := fmt.Sprintf("pod was not scheduled within %s", s.cfg.ScheduleTimeout)
			if st.message != "" {
				msg += ": " + st.message
			}
			return nil, false, errors.New(msg)

		case <-ticker.C:
			if msg := st.imagePullFailure(s.cfg.ImagePullTimeout); msg != "" {
				return nil, false, errors.New(msg)
			}

		case ev, ok := <-w.ResultChan():
			if !ok {
				return nil, true, nil
			}
			switch ev.Type {
			case watch.Deleted:
				return nil, false, errors.New("pod was deleted externally")
			case watch.Error:
				err := apierrors.FromObject(ev.Object)
				if apierrors.IsResourceExpired(err) || apierrors.IsGone(err) {
					return nil, true, nil
				}
				return nil, false, fmt.Errorf("watch error: %w", err)
			}
			p, ok := ev.Object.(*corev1.Pod)
			if !ok || p.Name != name {
				continue
			}
			s.observe(jobID, p, st)
			if isTerminal(p) {
				return p, false, nil
			}
			if msg := st.imagePullFailure(s.cfg.ImagePullTimeout); msg != "" {
				return nil, false, errors.New(msg)
			}
		}
	}
}

// observe copies useful in-flight facts (node, scheduling or image-pull
// problems) onto the job so GET /work/{id} explains what is going on, and
// updates the watcher's state.
func (s *Spawner) observe(jobID string, p *corev1.Pod, st *podState) {
	msg := ""
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
			msg = c.Reason
			if c.Message != "" {
				msg += ": " + c.Message
			}
		}
	}
	pullReason := ""
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Name != ContainerName || cs.State.Waiting == nil || cs.State.Waiting.Reason == "" {
			continue
		}
		msg = cs.State.Waiting.Reason
		if cs.State.Waiting.Message != "" {
			msg += ": " + cs.State.Waiting.Message
		}
		switch cs.State.Waiting.Reason {
		case "ErrImagePull", "ImagePullBackOff", "InvalidImageName", "ErrImageNeverPull":
			pullReason = cs.State.Waiting.Reason
		}
	}

	if p.Spec.NodeName != "" {
		st.scheduled = true
	}
	st.message = msg
	switch {
	case pullReason == "":
		st.pullFailingSince = time.Time{}
	case st.pullReason == "":
		st.pullFailingSince = time.Now()
	}
	st.pullReason = pullReason

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

// deletePod removes a worker pod. It requests a graceful delete, confirms the
// pod is gone, and force-deletes (grace period 0) a pod that stays
// Terminating past deleteWait — what happens when the node's kubelet is gone
// and can no longer acknowledge the deletion. Cleanup never depends on the
// (possibly cancelled) job context.
func (s *Spawner) deletePod(name string, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*s.cfg.deleteWait+s.cfg.APIRetryWindow)
	defer cancel()
	pods := s.client.CoreV1().Pods(s.cfg.Namespace)
	policy := metav1.DeletePropagationBackground

	del := func(grace *int64) error {
		return s.retry(ctx, "delete pod", func() error {
			err := pods.Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &policy, GracePeriodSeconds: grace})
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		})
	}

	if err := del(nil); err != nil {
		log.Error("failed to delete worker pod; the periodic sweep will retry", "err", err)
		return
	}
	gone, stuck := s.awaitGone(ctx, name)
	switch {
	case gone:
		log.Info("worker pod deleted")
	case stuck:
		zero := int64(0)
		if err := del(&zero); err != nil {
			log.Error("force delete failed; the periodic sweep will retry", "err", err)
			return
		}
		log.Warn("worker pod stayed Terminating (node gone?); force-deleted")
	default:
		log.Warn("worker pod still present after delete; the periodic sweep will retry")
	}
}

// awaitGone polls until the pod disappears. stuck reports that it is still
// present with a deletion timestamp once deleteWait has elapsed.
func (s *Spawner) awaitGone(ctx context.Context, name string) (gone, stuck bool) {
	pods := s.client.CoreV1().Pods(s.cfg.Namespace)
	deadline := time.Now().Add(s.cfg.deleteWait)
	for {
		p, err := pods.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, false
		}
		if time.Now().After(deadline) {
			return false, err == nil && p.DeletionTimestamp != nil
		}
		select {
		case <-ctx.Done():
			return false, false
		case <-time.After(s.cfg.pollInterval):
		}
	}
}

// Sweep deletes worker pods that no in-flight job owns: everything on
// start-up (job state is in memory, so nothing left by a previous instance
// can be collected), and later any pod whose job is unknown or already
// finished — for example one whose delete failed during an API-server
// outage. Failed pods kept by KeepFailedPods are left alone.
func (s *Spawner) Sweep(ctx context.Context) (int, error) {
	pods := s.client.CoreV1().Pods(s.cfg.Namespace)
	list, err := pods.List(ctx, metav1.ListOptions{LabelSelector: LabelApp + "=" + AppName})
	if err != nil {
		return 0, fmt.Errorf("list worker pods: %w", err)
	}
	n := 0
	for i := range list.Items {
		p := &list.Items[i]
		jobID := p.Labels[LabelJobID]
		if j, ok := s.store.Get(jobID); ok {
			if !j.Status.Terminal() || (s.cfg.KeepFailedPods && j.Status == store.StatusFailed) {
				continue
			}
		}
		s.log.Warn("deleting orphan worker pod", "pod", p.Name, "phase", p.Status.Phase, "job", jobID)
		s.deletePod(p.Name, s.log.With("pod", p.Name))
		n++
	}
	return n, nil
}

// RunSweeper calls Sweep every interval until ctx is cancelled.
func (s *Spawner) RunSweeper(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := s.Sweep(ctx); err != nil {
				s.log.Warn("sweep failed", "err", err)
			} else if n > 0 {
				s.log.Warn("sweep deleted orphan worker pods", "count", n)
			}
		}
	}
}

// retry runs op until it succeeds, fails permanently, or the retry window
// elapses. Only API-server/network-style failures are retried.
func (s *Spawner) retry(ctx context.Context, what string, op func() error) error {
	deadline := time.Now().Add(s.cfg.APIRetryWindow)
	delay := s.cfg.retryBase
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
		s.log.Warn("transient API error, retrying", "op", what, "attempt", attempt, "in", delay, "err", err)
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

// isTransient reports whether an API error is worth retrying: 5xx, server
// timeouts, throttling and connection failures are; 4xx responses that
// describe the request or the object's state are not.
func isTransient(err error) bool {
	switch {
	case apierrors.IsNotFound(err), apierrors.IsGone(err), apierrors.IsAlreadyExists(err),
		apierrors.IsConflict(err), apierrors.IsForbidden(err), apierrors.IsUnauthorized(err),
		apierrors.IsInvalid(err), apierrors.IsBadRequest(err), apierrors.IsMethodNotSupported(err),
		apierrors.IsNotAcceptable(err), apierrors.IsUnsupportedMediaType(err),
		apierrors.IsRequestEntityTooLargeError(err):
		return false
	}
	return true
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
