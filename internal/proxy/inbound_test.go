package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"sidecar/internal/config"
	"sidecar/internal/reqctx"
)

// app stands in for the local app, recording what inbound handed it.
type app struct {
	srv *httptest.Server

	mu   sync.Mutex
	last *http.Request
	body string
}

func newApp(t *testing.T, delay time.Duration) *app {
	t.Helper()
	a := &app{}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		a.mu.Lock()
		a.last, a.body = r.Clone(context.Background()), string(body)
		a.mu.Unlock()

		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *app) addr() string { return a.srv.Listener.Addr().String() }

func (a *app) received(t *testing.T) *http.Request {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.last == nil {
		t.Fatal("the app was never called")
	}
	return a.last
}

// timeouts are config's own defaults, so the tests read like the shipped config.
func timeouts() config.Inbound {
	return config.Inbound{DefaultTimeout: 3 * time.Second, MaxTimeout: 30 * time.Second}
}

func newInbound(t *testing.T, appAddr string, tm config.Inbound) (*httptest.Server, *accessLog) {
	t.Helper()
	al := newAccessLog()
	srv := httptest.NewServer(NewInbound(appAddr, tm, slog.New(al)))
	t.Cleanup(srv.Close)
	return srv, al
}

func callInbound(t *testing.T, sidecar *httptest.Server, method string, body io.Reader, h http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, sidecar.URL+"/v1/thing", body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	for k, vs := range h {
		req.Header[k] = vs
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("call inbound: %v", err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

func TestInboundForwardsToApp(t *testing.T) {
	a := newApp(t, 0)
	sidecar, _ := newInbound(t, a.addr(), timeouts())

	res := callInbound(t, sidecar, http.MethodPut, strings.NewReader("the body"), nil)

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	got := a.received(t)
	if got.Method != http.MethodPut {
		t.Errorf("method = %q, want PUT", got.Method)
	}
	if got.URL.Path != "/v1/thing" {
		t.Errorf("path = %q, want /v1/thing", got.URL.Path)
	}
	if a.body != "the body" {
		t.Errorf("body = %q, want %q", a.body, "the body")
	}
}

func TestInboundStampsRequestID(t *testing.T) {
	tests := []struct {
		name string
		sent string
		keep bool
	}{
		{"none sent", "", false},
		{"usable", "7f3a2b1c", true},
		// net/http refuses to send control characters either way, so what is left
		// to reject here is what a client can really put on the wire.
		{"too long", strings.Repeat("a", 200), false},
		{"non-ascii", "idą", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := newApp(t, 0)
			sidecar, al := newInbound(t, a.addr(), timeouts())

			h := http.Header{}
			if tc.sent != "" {
				h.Set(reqctx.HeaderRequestID, tc.sent)
			}
			callInbound(t, sidecar, http.MethodGet, nil, h)

			id := a.received(t).Header.Get(reqctx.HeaderRequestID)
			if id == "" {
				t.Fatal("the app got no request id")
			}
			if tc.keep != (id == tc.sent) {
				t.Errorf("app saw %q, sent %q, wanted it kept = %v", id, tc.sent, tc.keep)
			}
			// The line reports what the app was given, not what the caller sent.
			if got := field[string](t, al.next(t), "requestId"); got != id {
				t.Errorf("access line requestId = %q, want the stamped %q", got, id)
			}
		})
	}
}

func TestInboundStampsTraceparent(t *testing.T) {
	const caller = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

	t.Run("continues the caller's trace", func(t *testing.T) {
		a := newApp(t, 0)
		sidecar, al := newInbound(t, a.addr(), timeouts())

		callInbound(t, sidecar, http.MethodGet, nil, http.Header{reqctx.HeaderTraceparent: {caller}})

		got := a.received(t).Header.Get(reqctx.HeaderTraceparent)
		if reqctx.TraceID(got) != reqctx.TraceID(caller) {
			t.Errorf("app saw trace %q, want the caller's %q", reqctx.TraceID(got), reqctx.TraceID(caller))
		}
		if got == caller {
			t.Error("traceparent passed through unchanged, want a new span for this hop")
		}
		if line := field[string](t, al.next(t), "traceId"); line != reqctx.TraceID(caller) {
			t.Errorf("access line traceId = %q, want the caller's trace", line)
		}
	})

	t.Run("starts a trace when the caller sent none", func(t *testing.T) {
		a := newApp(t, 0)
		sidecar, al := newInbound(t, a.addr(), timeouts())

		callInbound(t, sidecar, http.MethodGet, nil, nil)

		if got := a.received(t).Header.Get(reqctx.HeaderTraceparent); reqctx.TraceID(got) == "" {
			t.Errorf("app saw traceparent %q, want a usable one", got)
		}
		if got := field[string](t, al.next(t), "traceId"); got == "" {
			t.Error("access line traceId is empty, want the trace this hop started")
		}
	})
}

func TestInboundGivesTheAppADeadline(t *testing.T) {
	tests := []struct {
		name   string
		sentMs string
		want   time.Duration
	}{
		{"caller sent none", "", 3 * time.Second},
		{"caller sent junk", "soon", 3 * time.Second},
		{"caller sent less", "500", 500 * time.Millisecond},
		{"caller sent more than allowed", "600000", 30 * time.Second},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := newApp(t, 0)
			sidecar, al := newInbound(t, a.addr(), timeouts())

			h := http.Header{}
			if tc.sentMs != "" {
				h.Set(reqctx.HeaderTimeoutMs, tc.sentMs)
			}
			before := time.Now()
			callInbound(t, sidecar, http.MethodGet, nil, h)

			if d := appDeadline(t, a).Sub(before); d < tc.want-time.Second || d > tc.want+time.Second {
				t.Errorf("app deadline is %v out, want about %v", d, tc.want)
			}
			if got := field[int64](t, al.next(t), "deadlineMs"); got != tc.want.Milliseconds() {
				t.Errorf("deadlineMs = %d, want %d", got, tc.want.Milliseconds())
			}
		})
	}
}

// The absolute form is host-local, so a caller on the network must not set it.
func TestInboundOverwritesCallerDeadline(t *testing.T) {
	a := newApp(t, 0)
	sidecar, _ := newInbound(t, a.addr(), timeouts())

	callInbound(t, sidecar, http.MethodGet, nil, http.Header{reqctx.HeaderDeadline: {"1"}})

	if got := a.received(t).Header.Get(reqctx.HeaderDeadline); got == "1" {
		t.Error("the caller's X-Sidecar-Deadline reached the app")
	}
}

func TestInboundDeadlineExceeded(t *testing.T) {
	a := newApp(t, 2*time.Second)
	tm := timeouts()
	tm.DefaultTimeout = 150 * time.Millisecond
	sidecar, al := newInbound(t, a.addr(), tm)

	start := time.Now()
	res := callInbound(t, sidecar, http.MethodGet, nil, nil)

	if res.StatusCode != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want 504", res.StatusCode)
	}
	if got := res.Header.Get("X-Sidecar-Error"); got != "deadline_exceeded" {
		t.Errorf("X-Sidecar-Error = %q, want deadline_exceeded", got)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %v, want the deadline to cut it short", elapsed)
	}
	if got := field[string](t, al.next(t), "sidecarError"); got != "deadline_exceeded" {
		t.Errorf("access line sidecarError = %q, want deadline_exceeded", got)
	}
}

func TestInboundAppUnavailable(t *testing.T) {
	sidecar, al := newInbound(t, closedAddr(t), timeouts())

	res := callInbound(t, sidecar, http.MethodGet, nil, nil)

	if res.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", res.StatusCode)
	}
	if got := res.Header.Get("X-Sidecar-Error"); got != "app_unavailable" {
		t.Errorf("X-Sidecar-Error = %q, want app_unavailable", got)
	}
	if got := field[string](t, al.next(t), "sidecarError"); got != "app_unavailable" {
		t.Errorf("access line sidecarError = %q, want app_unavailable", got)
	}
}

// Inbound has one app: nothing to retry onto, nothing to balance across.
func TestInboundAccessLine(t *testing.T) {
	a := newApp(t, 0)
	sidecar, al := newInbound(t, a.addr(), timeouts())

	callInbound(t, sidecar, http.MethodGet, nil, nil)

	line := al.next(t)
	if got := field[string](t, line, "dir"); got != "inbound" {
		t.Errorf("dir = %q, want inbound", got)
	}
	if got := field[int64](t, line, "attempts"); got != 0 {
		t.Errorf("attempts = %d, want 0", got)
	}
	if got := field[[]string](t, line, "instances"); len(got) != 0 {
		t.Errorf("instances = %v, want empty", got)
	}
	if got := field[int64](t, line, "status"); got != 200 {
		t.Errorf("status = %d, want 200", got)
	}
}

func appDeadline(t *testing.T, a *app) time.Time {
	t.Helper()
	raw := a.received(t).Header.Get(reqctx.HeaderDeadline)
	ms, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		t.Fatalf("app got X-Sidecar-Deadline %q, want unix ms: %v", raw, err)
	}
	return time.UnixMilli(ms)
}
