// Package store keeps in-memory state for work requests.
//
// This is deliberately simple for a PoC: state lives in the controller
// process and is lost on restart. A real system would persist this in a
// database or represent each request as a Kubernetes object (CRD / Job).
package store

import (
	"sort"
	"sync"
	"time"
)

// Status is the lifecycle state of a work request.
type Status string

const (
	StatusPending   Status = "Pending"   // accepted, pod not yet created (or queued)
	StatusRunning   Status = "Running"   // pod created, waiting for completion
	StatusSucceeded Status = "Succeeded" // pod exited 0
	StatusFailed    Status = "Failed"    // pod exited non-zero, timed out, or errored
)

// Terminal reports whether no further transitions are expected.
func (s Status) Terminal() bool {
	return s == StatusSucceeded || s == StatusFailed
}

// WorkSpec is what the caller asks us to run.
type WorkSpec struct {
	Image   string   `json:"image"`
	Command []string `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	// TimeoutSeconds bounds pod runtime; 0 means use the controller default.
	TimeoutSeconds int64 `json:"timeoutSeconds,omitempty"`
}

// Job is a single work request and its observed outcome.
type Job struct {
	ID       string   `json:"id"`
	Spec     WorkSpec `json:"spec"`
	Status   Status   `json:"status"`
	PodName  string   `json:"podName,omitempty"`
	NodeName string   `json:"nodeName,omitempty"`
	// Message carries the latest scheduling / image-pull hint while the job
	// is in flight, e.g. "Unschedulable: 0/3 nodes are available ...".
	Message   string     `json:"message,omitempty"`
	ExitCode  *int32     `json:"exitCode,omitempty"`
	Logs      string     `json:"logs,omitempty"`
	Error     string     `json:"error,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
	StartedAt *time.Time `json:"startedAt,omitempty"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`
}

// Store is a concurrency-safe map of jobs.
type Store struct {
	mu   sync.RWMutex
	jobs map[string]*Job
}

func New() *Store {
	return &Store{jobs: make(map[string]*Job)}
}

// Put inserts or replaces a job.
func (s *Store) Put(j *Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *j
	s.jobs[j.ID] = &cp
}

// Get returns a copy of the job, so callers can't mutate shared state.
func (s *Store) Get(id string) (*Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.jobs[id]
	if !ok {
		return nil, false
	}
	cp := *j
	return &cp, true
}

// Update applies fn to the stored job under the lock.
func (s *Store) Update(id string, fn func(*Job)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return false
	}
	fn(j)
	return true
}

// List returns copies of all jobs, newest first.
func (s *Store) List() []*Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		cp := *j
		out = append(out, &cp)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].CreatedAt.After(out[b].CreatedAt) })
	return out
}
