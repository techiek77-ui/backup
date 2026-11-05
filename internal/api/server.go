// Package api exposes the HTTP interface of the controller.
//
//	POST /work        submit work, returns 202 + {"id": ...}
//	GET  /work        list jobs (newest first)
//	GET  /work/{id}   job status, exit code and logs
//	GET  /sizes       configured size classes
//	GET  /healthz     liveness
//	GET  /readyz      readiness
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/techiek77-ui/backup/internal/config"
	"github.com/techiek77-ui/backup/internal/store"
)

// Runner executes one job to completion (see spawner.Spawner).
type Runner interface {
	Run(ctx context.Context, jobID string)
}

type Options struct {
	// MaxConcurrent caps simultaneously running worker pods; extra requests
	// are queued (status stays Pending) rather than rejected.
	MaxConcurrent int
	DefaultImage  string
	MaxBodyBytes  int64
	// Classes are the accepted values of the request's "size" field;
	// DefaultClass is applied when it is empty.
	Classes      map[string]config.SizeClass
	DefaultClass string
	// MaxTimeout caps a request's timeoutSeconds; zero means no cap.
	MaxTimeout time.Duration
}

type Server struct {
	store  *store.Store
	runner Runner
	opts   Options
	log    *slog.Logger

	base context.Context
	sem  chan struct{}
	wg   sync.WaitGroup
}

// New wires a Server. base is the lifetime context for all jobs: cancelling
// it aborts in-flight jobs (their pods get deleted by the Runner).
func New(base context.Context, st *store.Store, runner Runner, opts Options, log *slog.Logger) *Server {
	if opts.MaxConcurrent < 1 {
		opts.MaxConcurrent = 1
	}
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = 64 * 1024
	}
	if len(opts.Classes) == 0 {
		opts.Classes = config.DefaultSizeClasses()
	}
	if _, ok := opts.Classes[opts.DefaultClass]; !ok {
		opts.DefaultClass = config.ClassNames(opts.Classes)[0]
	}
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		store:  st,
		runner: runner,
		opts:   opts,
		log:    log,
		base:   base,
		sem:    make(chan struct{}, opts.MaxConcurrent),
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("POST /work", s.handleSubmit)
	mux.HandleFunc("GET /work", s.handleList)
	mux.HandleFunc("GET /work/{id}", s.handleGet)
	mux.HandleFunc("GET /sizes", s.handleSizes)
	return logging(s.log, mux)
}

// Wait blocks until all job goroutines have exited or ctx is done.
func (s *Server) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"service": "podspawner",
		"endpoints": []string{
			"POST /work  {\"image\":\"busybox:1.36\",\"command\":[\"sh\",\"-c\",\"echo hi\"],\"size\":\"" + s.opts.DefaultClass + "\"}",
			"GET  /work",
			"GET  /work/{id}",
			"GET  /sizes",
		},
	})
}

func (s *Server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	var spec store.WorkSpec
	body := http.MaxBytesReader(w, r.Body, s.opts.MaxBodyBytes)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if dec.More() {
		writeError(w, http.StatusBadRequest, "unexpected trailing data")
		return
	}
	if spec.Image = strings.TrimSpace(spec.Image); spec.Image == "" {
		spec.Image = s.opts.DefaultImage
	}
	if spec.Image == "" {
		writeError(w, http.StatusBadRequest, "image is required")
		return
	}
	if spec.TimeoutSeconds < 0 {
		writeError(w, http.StatusBadRequest, "timeoutSeconds must be >= 0")
		return
	}
	if max := int64(s.opts.MaxTimeout.Seconds()); max > 0 && spec.TimeoutSeconds > max {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("timeoutSeconds must be <= %d (WORKER_MAX_TIMEOUT=%s)", max, s.opts.MaxTimeout))
		return
	}
	if spec.Size = strings.TrimSpace(spec.Size); spec.Size == "" {
		spec.Size = s.opts.DefaultClass
	}
	if _, ok := s.opts.Classes[spec.Size]; !ok {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown size %q; valid sizes: %s", spec.Size, strings.Join(config.ClassNames(s.opts.Classes), ", ")))
		return
	}

	id, err := newID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not generate id")
		return
	}
	job := &store.Job{
		ID:        id,
		Spec:      spec,
		Status:    store.StatusPending,
		CreatedAt: time.Now(),
	}
	s.store.Put(job)
	s.dispatch(id)

	w.Header().Set("Location", "/work/"+id)
	writeJSON(w, http.StatusAccepted, job)
}

// dispatch runs the job in the background, respecting the concurrency cap.
func (s *Server) dispatch(id string) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		select {
		case s.sem <- struct{}{}:
		case <-s.base.Done():
			ended := time.Now()
			s.store.Update(id, func(j *store.Job) {
				j.Status = store.StatusFailed
				j.Error = "controller shutting down before job started"
				j.EndedAt = &ended
			})
			return
		}
		defer func() { <-s.sem }()
		s.runner.Run(s.base, id)
	}()
}

type sizeView struct {
	CPU          string            `json:"cpu"`
	Memory       string            `json:"memory"`
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	Tolerations  int               `json:"tolerations,omitempty"`
	NodePoolID   string            `json:"nodePoolId,omitempty"`
}

func (s *Server) handleSizes(w http.ResponseWriter, _ *http.Request) {
	out := map[string]sizeView{}
	for name, c := range s.opts.Classes {
		out[name] = sizeView{CPU: c.CPU.String(), Memory: c.Memory.String(), NodeSelector: c.NodeSelector, Tolerations: len(c.Tolerations), NodePoolID: c.NodePoolID}
	}
	writeJSON(w, http.StatusOK, map[string]any{"default": s.opts.DefaultClass, "classes": out})
}

func (s *Server) handleList(w http.ResponseWriter, _ *http.Request) {
	jobs := s.store.List()
	// Logs can be large; keep the listing light.
	for _, j := range jobs {
		j.Logs = ""
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": jobs})
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	job, ok := s.store.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "no such job")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func newID() (string, error) {
	b := make([]byte, 6)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

func logging(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)
		log.Info("http", "method", r.Method, "path", r.URL.Path, "status", rec.code,
			"dur", fmt.Sprintf("%.1fms", float64(time.Since(start).Microseconds())/1000))
	})
}
