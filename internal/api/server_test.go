package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/techiek77-ui/backup/internal/store"
)

// stubRunner marks jobs Succeeded after an optional delay and records the
// peak number of concurrent Run calls.
type stubRunner struct {
	store   *store.Store
	delay   time.Duration
	running atomic.Int32
	peak    atomic.Int32
	mu      sync.Mutex
	seen    []string
}

func (r *stubRunner) Run(ctx context.Context, id string) {
	n := r.running.Add(1)
	defer r.running.Add(-1)
	for {
		p := r.peak.Load()
		if n <= p || r.peak.CompareAndSwap(p, n) {
			break
		}
	}
	r.mu.Lock()
	r.seen = append(r.seen, id)
	r.mu.Unlock()

	select {
	case <-time.After(r.delay):
	case <-ctx.Done():
	}
	now := time.Now()
	r.store.Update(id, func(j *store.Job) {
		j.Status = store.StatusSucceeded
		j.Logs = "hello from " + id
		j.EndedAt = &now
	})
}

func newTestServer(t *testing.T, opts Options, delay time.Duration) (*httptest.Server, *Server, *stubRunner) {
	t.Helper()
	st := store.New()
	r := &stubRunner{store: st, delay: delay}
	ctx, cancel := context.WithCancel(context.Background())
	s := New(ctx, st, r, opts, slog.New(slog.NewTextHandler(discard{}, nil)))
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() { ts.Close(); cancel() })
	return ts, s, r
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func post(t *testing.T, ts *httptest.Server, body string) (*http.Response, map[string]any) {
	t.Helper()
	resp, err := http.Post(ts.URL+"/work", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func get(t *testing.T, ts *httptest.Server, path string) (*http.Response, map[string]any) {
	t.Helper()
	resp, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func TestSubmitAndPoll(t *testing.T) {
	ts, s, _ := newTestServer(t, Options{MaxConcurrent: 2, DefaultImage: "busybox:1.36"}, 0)

	resp, body := post(t, ts, `{"command":["sh","-c","echo hi"]}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d body=%v", resp.StatusCode, body)
	}
	id, _ := body["id"].(string)
	if len(id) != 12 {
		t.Fatalf("id = %q", id)
	}
	if resp.Header.Get("Location") != "/work/"+id {
		t.Errorf("Location = %q", resp.Header.Get("Location"))
	}
	if spec := body["spec"].(map[string]any); spec["image"] != "busybox:1.36" {
		t.Errorf("default image not applied: %v", spec)
	}

	// Wait for the runner goroutine.
	if err := s.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	resp, job := get(t, ts, "/work/"+id)
	if resp.StatusCode != 200 || job["status"] != "Succeeded" || job["logs"] != "hello from "+id {
		t.Fatalf("got %d %v", resp.StatusCode, job)
	}

	// Listing omits logs.
	_, list := get(t, ts, "/work")
	items := list["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["logs"] != nil {
		t.Fatalf("list = %v", list)
	}
}

func TestSubmitValidation(t *testing.T) {
	ts, _, _ := newTestServer(t, Options{MaxConcurrent: 1}, 0) // no default image

	cases := map[string]string{
		"not json":         `{`,
		"unknown field":    `{"image":"x","bogus":1}`,
		"no image":         `{"command":["true"]}`,
		"negative timeout": `{"image":"x","timeoutSeconds":-1}`,
		"trailing garbage": `{"image":"x"} {}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			resp, out := post(t, ts, body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d body=%v", resp.StatusCode, out)
			}
			if out["error"] == nil {
				t.Fatalf("expected error message, got %v", out)
			}
		})
	}
}

func TestGetUnknown(t *testing.T) {
	ts, _, _ := newTestServer(t, Options{MaxConcurrent: 1}, 0)
	resp, _ := get(t, ts, "/work/doesnotexist")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestConcurrencyCapQueuesRequests(t *testing.T) {
	ts, s, r := newTestServer(t, Options{MaxConcurrent: 2}, 150*time.Millisecond)

	var ids []string
	for i := 0; i < 6; i++ {
		resp, body := post(t, ts, `{"image":"busybox"}`)
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("submit %d: %d", i, resp.StatusCode)
		}
		ids = append(ids, body["id"].(string))
	}
	if err := s.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p := r.peak.Load(); p > 2 {
		t.Fatalf("peak concurrency = %d, want <= 2", p)
	}
	if len(r.seen) != 6 {
		t.Fatalf("ran %d jobs, want 6", len(r.seen))
	}
	for _, id := range ids {
		if _, job := get(t, ts, "/work/"+id); job["status"] != "Succeeded" {
			t.Errorf("%s: %v", id, job["status"])
		}
	}
}

func TestShutdownFailsQueuedJobs(t *testing.T) {
	st := store.New()
	r := &stubRunner{store: st, delay: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	s := New(ctx, st, r, Options{MaxConcurrent: 1}, slog.New(slog.NewTextHandler(discard{}, nil)))
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	_, first := post(t, ts, `{"image":"busybox"}`)
	_, second := post(t, ts, `{"image":"busybox"}`)
	time.Sleep(50 * time.Millisecond) // let the first job occupy the slot
	cancel()
	if err := s.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if j, _ := st.Get(second["id"].(string)); j.Status != store.StatusFailed || !strings.Contains(j.Error, "shutting down") {
		t.Fatalf("queued job should fail on shutdown, got %+v", j)
	}
	if j, _ := st.Get(first["id"].(string)); j.Status != store.StatusSucceeded {
		t.Fatalf("running job should have been handed the cancelled ctx and finished, got %+v", j)
	}
}
