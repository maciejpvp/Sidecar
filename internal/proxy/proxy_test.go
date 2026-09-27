package proxy

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sidecar/internal/config"
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

func service(instances ...string) config.Service {
	return config.NewService("svc", instances...)
}

func newSidecar(t *testing.T, svc config.Service) *httptest.Server {
	t.Helper()
	srv, _ := newSidecarLogging(t, svc)
	return srv
}

// newSidecarLogging is newSidecar plus the access lines it writes.
func newSidecarLogging(t *testing.T, svc config.Service) (*httptest.Server, *accessLog) {
	t.Helper()
	al := newAccessLog()
	table := routing.NewTable([]config.Service{svc})
	srv := httptest.NewServer(New(table, slog.New(al)))
	t.Cleanup(srv.Close)
	return srv, al
}

func call(t *testing.T, sidecar *httptest.Server, method string, body io.Reader) *http.Response {
	t.Helper()
	return callHost(t, sidecar, "svc", method, body)
}

// callHost is call addressed to a service name of the test's choosing, for the
// cases where the name itself is the point.
func callHost(t *testing.T, sidecar *httptest.Server, host, method string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, sidecar.URL+"/v1/thing", body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Host = host
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("call sidecar: %v", err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

// accessLog captures the access lines a sidecar writes (DESIGN §11). A channel
// rather than a buffer: the line is emitted on the server's goroutine after the
// client already holds the response, so receiving is the synchronisation point
// that a time.Sleep would only paper over.
type accessLog struct {
	lines chan map[string]any
}

func newAccessLog() *accessLog {
	return &accessLog{lines: make(chan map[string]any, 8)}
}

func (a *accessLog) Enabled(context.Context, slog.Level) bool { return true }

func (a *accessLog) Handle(_ context.Context, r slog.Record) error {
	if r.Message != "access" {
		return nil
	}

	line := make(map[string]any, r.NumAttrs())
	r.Attrs(func(at slog.Attr) bool {
		line[at.Key] = at.Value.Any()
		return true
	})

	select {
	case a.lines <- line:
	default: // a test that ignores the log must never wedge the handler
	}
	return nil
}

func (a *accessLog) WithAttrs([]slog.Attr) slog.Handler { return a }
func (a *accessLog) WithGroup(string) slog.Handler      { return a }

// next returns the access line for one request, failing instead of hanging.
func (a *accessLog) next(t *testing.T) map[string]any {
	t.Helper()
	select {
	case line := <-a.lines:
		return line
	case <-time.After(2 * time.Second):
		t.Fatal("no access line was written")
		return nil
	}
}

// field reads one field of an access line, failing if it is missing or is not
// the type §11 says it is.
func field[T any](t *testing.T, line map[string]any, key string) T {
	t.Helper()
	v, ok := line[key].(T)
	if !ok {
		t.Fatalf("%s = %#v, want a %T", key, line[key], v)
	}
	return v
}

func TestRetriesOntoHealthyInstance(t *testing.T) {
	bad := newUpstream(t, http.StatusServiceUnavailable)
	good := newUpstream(t, http.StatusOK)

	sidecar := newSidecar(t, service(bad.addr(), good.addr()))

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

	svc := service(a.addr(), b.addr(), c.addr())
	svc.Retry.MaxAttempts = 3
	sidecar := newSidecar(t, svc)

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

	svc := service(a.addr(), b.addr())
	svc.Retry.MaxAttempts = 5
	sidecar := newSidecar(t, svc)

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

	svc := service(only.addr())
	svc.Retry.MaxAttempts = 3
	sidecar := newSidecar(t, svc)

	call(t, sidecar, http.MethodGet, nil)

	if got := only.hits.Load(); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
}

func TestSingleInstanceRecovers(t *testing.T) {
	flaky := newFlakyUpstream(t, 1)

	svc := service(flaky.addr())
	svc.Retry.MaxAttempts = 3
	sidecar := newSidecar(t, svc)

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

			sidecar := newSidecar(t, service(first.addr(), second.addr()))

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

	sidecar := newSidecar(t, service(bad.addr(), good.addr()))

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

	sidecar := newSidecar(t, service(bad.addr(), good.addr()))

	res := call(t, sidecar, http.MethodPut, strings.NewReader(payload))

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if got := good.received(); len(got) != 1 || got[0] != payload {
		t.Errorf("retried body = %q, want [%q]", got, payload)
	}
}

// A body over the service's retry.maxBodyBytes is streamed, so it is not retried.
func TestReplayLimitComesFromService(t *testing.T) {
	bad := newUpstream(t, http.StatusServiceUnavailable)
	good := newUpstream(t, http.StatusOK)

	svc := service(bad.addr(), good.addr())
	svc.Retry.MaxBodyBytes = 4
	sidecar := newSidecar(t, svc)

	res := call(t, sidecar, http.MethodPut, strings.NewReader("longer than four bytes"))

	if res.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (not retried)", res.StatusCode)
	}
	if got := good.hits.Load(); got != 0 {
		t.Errorf("second instance hits = %d, want 0 (body over retry.maxBodyBytes)", got)
	}
}

// A body of unknown length is streamed, so there is nothing to replay and the
// request must not be retried.
func TestDoesNotRetryUnbufferedBody(t *testing.T) {
	bad := newUpstream(t, http.StatusServiceUnavailable)
	good := newUpstream(t, http.StatusOK)

	sidecar := newSidecar(t, service(bad.addr(), good.addr()))

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

	sidecar := newSidecar(t, service(dead, good.addr()))

	res := call(t, sidecar, http.MethodPost, strings.NewReader("safe: never sent"))

	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", res.StatusCode)
	}
	if got := good.hits.Load(); got != 1 {
		t.Errorf("healthy instance hits = %d, want 1", got)
	}
}

func TestAllInstancesUnreachable(t *testing.T) {
	sidecar := newSidecar(t, service(closedAddr(t), closedAddr(t)))

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

	svc := service(newSlow(), newSlow())
	svc.Timeout = 150 * time.Millisecond
	sidecar := newSidecar(t, svc)

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

// One line per request, whatever the outcome (DESIGN §11). The cases below are
// the outcomes that differ in what the line has to say.

func TestAccessLogHappyPath(t *testing.T) {
	up := newUpstream(t, http.StatusOK)

	svc := service(up.addr())
	svc.Timeout = 2 * time.Second // not the default, so deadlineMs proves it tracks the service
	sidecar, al := newSidecarLogging(t, svc)

	call(t, sidecar, http.MethodGet, nil)

	line := al.next(t)
	for key, want := range map[string]string{
		"dir": "outbound", "target": "svc", "method": http.MethodGet,
		"path": "/v1/thing", "sidecarError": "",
	} {
		if got := field[string](t, line, key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	for key, want := range map[string]int64{"status": 200, "attempts": 1, "deadlineMs": 2000} {
		if got := field[int64](t, line, key); got != want {
			t.Errorf("%s = %d, want %d", key, got, want)
		}
	}
	if got := field[[]string](t, line, "instances"); !slices.Equal(got, []string{up.addr()}) {
		t.Errorf("instances = %v, want [%s]", got, up.addr())
	}
}

// A name the table does not hold never reaches an instance, and the line says
// so: no attempts, no instances, and the name that was asked for.
func TestAccessLogNoRoute(t *testing.T) {
	sidecar, al := newSidecarLogging(t, service(newUpstream(t, http.StatusOK).addr()))

	res := callHost(t, sidecar, "nope", http.MethodGet, nil)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.StatusCode)
	}

	line := al.next(t)
	if got := field[string](t, line, "sidecarError"); got != "no_route" {
		t.Errorf("sidecarError = %q, want no_route", got)
	}
	if got := field[string](t, line, "target"); got != "nope" {
		t.Errorf("target = %q, want nope", got)
	}
	for key, want := range map[string]int64{"status": 404, "attempts": 0} {
		if got := field[int64](t, line, key); got != want {
			t.Errorf("%s = %d, want %d", key, got, want)
		}
	}
	if got := field[[]string](t, line, "instances"); len(got) != 0 {
		t.Errorf("instances = %v, want empty", got)
	}
}

// A sidecar with no snapshot yet logs the request it could not route like any
// other, and its deadline is 0 because no service policy was ever found.
func TestAccessLogMeshNotReady(t *testing.T) {
	al := newAccessLog()
	sidecar := httptest.NewServer(New(routing.NewStore(nil), slog.New(al)))
	t.Cleanup(sidecar.Close)

	res := call(t, sidecar, http.MethodGet, nil)
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.StatusCode)
	}

	line := al.next(t)
	if got := field[string](t, line, "sidecarError"); got != "mesh_not_ready" {
		t.Errorf("sidecarError = %q, want mesh_not_ready", got)
	}
	for key, want := range map[string]int64{"status": 503, "attempts": 0, "deadlineMs": 0} {
		if got := field[int64](t, line, key); got != want {
			t.Errorf("%s = %d, want %d", key, got, want)
		}
	}
}

// This is the line the whole feature is for: which instances a retried request
// actually touched.
func TestAccessLogRecordsEveryAttempt(t *testing.T) {
	bad := newUpstream(t, http.StatusServiceUnavailable)
	good := newUpstream(t, http.StatusOK)

	sidecar, al := newSidecarLogging(t, service(bad.addr(), good.addr()))

	call(t, sidecar, http.MethodGet, nil)

	line := al.next(t)
	for key, want := range map[string]int64{"status": 200, "attempts": 2} {
		if got := field[int64](t, line, key); got != want {
			t.Errorf("%s = %d, want %d", key, got, want)
		}
	}
	instances := field[[]string](t, line, "instances")
	if !slices.Contains(instances, bad.addr()) || !slices.Contains(instances, good.addr()) {
		t.Errorf("instances = %v, want the failed %s and the serving %s", instances, bad.addr(), good.addr())
	}
}

func TestAccessLogDeadlineExceeded(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(slow.Close)

	svc := service(slow.Listener.Addr().String())
	svc.Timeout = 150 * time.Millisecond
	sidecar, al := newSidecarLogging(t, svc)

	call(t, sidecar, http.MethodGet, nil)

	line := al.next(t)
	if got := field[string](t, line, "sidecarError"); got != "deadline_exceeded" {
		t.Errorf("sidecarError = %q, want deadline_exceeded", got)
	}
	for key, want := range map[string]int64{"status": 504, "deadlineMs": 150} {
		if got := field[int64](t, line, key); got != want {
			t.Errorf("%s = %d, want %d", key, got, want)
		}
	}
	if got := field[int64](t, line, "durationMs"); got < 150 {
		t.Errorf("durationMs = %d, want at least the 150ms the request was given", got)
	}
}
