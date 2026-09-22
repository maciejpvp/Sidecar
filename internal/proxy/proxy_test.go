package proxy

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sidecar/internal/routing"
)

// Addressing, round-robin and deadlines are covered end to end in ./e2e.
// These tests are about what the attempt loop does with a failed attempt.

type upstream struct {
	srv  *httptest.Server
	hits atomic.Int64

	mu     sync.Mutex
	bodies []string
}

// newUpstream answers every request with status, recording what it received.
func newUpstream(t *testing.T, status int) *upstream {
	t.Helper()
	u := &upstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.bodies = append(u.bodies, string(body))
		u.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

// newFlakyUpstream fails the first n requests with 503, then recovers.
func newFlakyUpstream(t *testing.T, n int64) *upstream {
	t.Helper()
	u := &upstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u.hits.Add(1) <= n {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

// addr is host:port, the form a routing table stores an instance in.
func (u *upstream) addr() string { return u.srv.Listener.Addr().String() }

func (u *upstream) received() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.bodies...)
}

// closedAddr is an address nothing is listening on, so dials fail immediately.
func closedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func newSidecar(t *testing.T, cfg routing.ServiceConfig) *httptest.Server {
	t.Helper()
	table, err := routing.NewTable(map[string]routing.ServiceConfig{"svc": cfg})
	if err != nil {
		t.Fatalf("build table: %v", err)
	}
	srv := httptest.NewServer(New(table, slog.New(slog.DiscardHandler)))
	t.Cleanup(srv.Close)
	return srv
}

func call(t *testing.T, sidecar *httptest.Server, method string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, sidecar.URL+"/v1/thing", body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Host = "svc"
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("call sidecar: %v", err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

func TestRetriesOntoHealthyInstance(t *testing.T) {
	bad := newUpstream(t, http.StatusServiceUnavailable)
	good := newUpstream(t, http.StatusOK)

	sidecar := newSidecar(t, routing.ServiceConfig{
		Instances: []string{bad.addr(), good.addr()},
	})

	res := call(t, sidecar, http.MethodGet, nil)

	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", res.StatusCode)
	}
	if got := bad.hits.Load(); got != 1 {
		t.Errorf("failing instance hits = %d, want 1", got)
	}
	if got := good.hits.Load(); got != 1 {
		t.Errorf("healthy instance hits = %d, want 1", got)
	}
}

func TestGivesUpAfterMaxAttempts(t *testing.T) {
	a := newUpstream(t, http.StatusServiceUnavailable)
	b := newUpstream(t, http.StatusServiceUnavailable)
	c := newUpstream(t, http.StatusServiceUnavailable)

	sidecar := newSidecar(t, routing.ServiceConfig{
		Instances:   []string{a.addr(), b.addr(), c.addr()},
		MaxAttempts: 3,
	})

	res := call(t, sidecar, http.MethodGet, nil)

	// The last real upstream response is returned, not a sidecar-invented error.
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", res.StatusCode)
	}
	if res.Header.Get("X-Sidecar-Error") != "" {
		t.Errorf("X-Sidecar-Error = %q, want empty", res.Header.Get("X-Sidecar-Error"))
	}
	total := a.hits.Load() + b.hits.Load() + c.hits.Load()
	if total != 3 {
		t.Errorf("total attempts = %d, want 3", total)
	}
}

// Once every instance has had a turn the loop starts a new lap, so MaxAttempts
// is the only cap on how many attempts a request gets.
func TestLapsOverInstances(t *testing.T) {
	a := newUpstream(t, http.StatusServiceUnavailable)
	b := newUpstream(t, http.StatusServiceUnavailable)

	sidecar := newSidecar(t, routing.ServiceConfig{
		Instances:   []string{a.addr(), b.addr()},
		MaxAttempts: 5,
	})

	call(t, sidecar, http.MethodGet, nil)

	if total := a.hits.Load() + b.hits.Load(); total != 5 {
		t.Errorf("total attempts = %d, want 5", total)
	}
	// Attempts stay spread across instances rather than hammering one.
	if a.hits.Load() == 0 || b.hits.Load() == 0 {
		t.Errorf("hits a=%d b=%d, want both instances used", a.hits.Load(), b.hits.Load())
	}
}

// A service with one instance is the common case in development, and the
// retry that matters most there — a refused connection while the app restarts
// — has only that instance to go back to.
func TestSingleInstanceStillRetries(t *testing.T) {
	only := newUpstream(t, http.StatusServiceUnavailable)

	sidecar := newSidecar(t, routing.ServiceConfig{
		Instances:   []string{only.addr()},
		MaxAttempts: 3,
	})

	call(t, sidecar, http.MethodGet, nil)

	if got := only.hits.Load(); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
}

func TestSingleInstanceRecovers(t *testing.T) {
	flaky := newFlakyUpstream(t, 1)

	sidecar := newSidecar(t, routing.ServiceConfig{
		Instances:   []string{flaky.addr()},
		MaxAttempts: 3,
	})

	res := call(t, sidecar, http.MethodGet, nil)

	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", res.StatusCode)
	}
	if got := flaky.hits.Load(); got != 2 {
		t.Errorf("attempts = %d, want 2 (one failure, then success)", got)
	}
}

func TestNonRetriableStatusesPassThrough(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{"application bug", http.StatusInternalServerError},
		{"rate limited", http.StatusTooManyRequests},
		{"not found", http.StatusNotFound},
		{"success", http.StatusOK},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			first := newUpstream(t, tc.status)
			second := newUpstream(t, http.StatusOK)

			sidecar := newSidecar(t, routing.ServiceConfig{
				Instances: []string{first.addr(), second.addr()},
			})

			res := call(t, sidecar, http.MethodGet, nil)

			if res.StatusCode != tc.status {
				t.Errorf("status = %d, want %d", res.StatusCode, tc.status)
			}
			if got := first.hits.Load(); got != 1 {
				t.Errorf("first instance hits = %d, want 1", got)
			}
			if got := second.hits.Load(); got != 0 {
				t.Errorf("second instance hits = %d, want 0 (no retry)", got)
			}
		})
	}
}

func TestDoesNotRetryPOST(t *testing.T) {
	bad := newUpstream(t, http.StatusServiceUnavailable)
	good := newUpstream(t, http.StatusOK)

	sidecar := newSidecar(t, routing.ServiceConfig{
		Instances: []string{bad.addr(), good.addr()},
	})

	res := call(t, sidecar, http.MethodPost, strings.NewReader("charge the card"))

	if res.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", res.StatusCode)
	}
	if got := good.hits.Load(); got != 0 {
		t.Errorf("second instance hits = %d, want 0 (POST is not idempotent)", got)
	}
}

func TestReplaysBufferedBody(t *testing.T) {
	const payload = "the whole body, not a drained one"

	bad := newUpstream(t, http.StatusServiceUnavailable)
	good := newUpstream(t, http.StatusOK)

	sidecar := newSidecar(t, routing.ServiceConfig{
		Instances: []string{bad.addr(), good.addr()},
	})

	res := call(t, sidecar, http.MethodPut, strings.NewReader(payload))

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if got := good.received(); len(got) != 1 || got[0] != payload {
		t.Errorf("retried body = %q, want [%q]", got, payload)
	}
}

// A body of unknown length is streamed, so there is nothing to replay and the
// request must not be retried.
func TestDoesNotRetryUnbufferedBody(t *testing.T) {
	bad := newUpstream(t, http.StatusServiceUnavailable)
	good := newUpstream(t, http.StatusOK)

	sidecar := newSidecar(t, routing.ServiceConfig{
		Instances: []string{bad.addr(), good.addr()},
	})

	// io.NopCloser hides the concrete type, so ContentLength stays unknown and
	// the request goes out chunked.
	res := call(t, sidecar, http.MethodPut, io.NopCloser(strings.NewReader("streamed")))

	if res.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", res.StatusCode)
	}
	if got := good.hits.Load(); got != 0 {
		t.Errorf("second instance hits = %d, want 0 (body cannot be replayed)", got)
	}
}

// A refused dial means the request never left, so even a POST is safe to retry.
func TestRetriesDialFailureForAnyMethod(t *testing.T) {
	dead := closedAddr(t)
	good := newUpstream(t, http.StatusOK)

	sidecar := newSidecar(t, routing.ServiceConfig{
		Instances: []string{dead, good.addr()},
	})

	res := call(t, sidecar, http.MethodPost, strings.NewReader("safe: never sent"))

	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", res.StatusCode)
	}
	if got := good.hits.Load(); got != 1 {
		t.Errorf("healthy instance hits = %d, want 1", got)
	}
}

func TestAllInstancesUnreachable(t *testing.T) {
	sidecar := newSidecar(t, routing.ServiceConfig{
		Instances: []string{closedAddr(t), closedAddr(t)},
	})

	res := call(t, sidecar, http.MethodGet, nil)

	if res.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", res.StatusCode)
	}
	if got := res.Header.Get("X-Sidecar-Error"); got != "upstream_connect_failed" {
		t.Errorf("X-Sidecar-Error = %q, want upstream_connect_failed", got)
	}
}

// Backoff must not push the request past its own deadline.
func TestRetriesStopAtDeadline(t *testing.T) {
	// Two separate instances, because a pool may not list the same address twice.
	newSlow := func() string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-time.After(2 * time.Second):
			case <-r.Context().Done():
			}
		}))
		t.Cleanup(srv.Close)
		return srv.Listener.Addr().String()
	}

	sidecar := newSidecar(t, routing.ServiceConfig{
		Instances: []string{newSlow(), newSlow()},
		Timeout:   150 * time.Millisecond,
	})

	start := time.Now()
	res := call(t, sidecar, http.MethodGet, nil)
	elapsed := time.Since(start)

	if res.StatusCode != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want 504", res.StatusCode)
	}
	if got := res.Header.Get("X-Sidecar-Error"); got != "deadline_exceeded" {
		t.Errorf("X-Sidecar-Error = %q, want deadline_exceeded", got)
	}
	if elapsed > time.Second {
		t.Errorf("took %v, want the deadline to cut it short", elapsed)
	}
}
