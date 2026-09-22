package e2e

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"sidecar/internal/routing"
)

func instances(urls ...string) routing.ServiceConfig {
	return routing.ServiceConfig{Instances: urls}
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func startSidecar(t *testing.T, routes map[string]routing.ServiceConfig) *Sidecar {
	t.Helper()
	s, err := StartSidecar("127.0.0.1:0", routes, quietLogger())
	if err != nil {
		t.Fatalf("start sidecar: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func startEcho(t *testing.T, name string) *Echo {
	t.Helper()
	e := StartEcho(name)
	t.Cleanup(e.Close)
	return e
}

// App A calls service B by name; B receives the request unchanged.
func TestRequestFromAToB(t *testing.T) {
	tests := []struct {
		name string
		call func(sidecarAddr, service, path string) (*http.Response, error)
		path string
	}{
		{name: "host header", call: Call, path: "/v1/hello"},
		{name: "proxy style", call: CallViaProxy, path: "/v1/hello"},
		{name: "path kept verbatim", call: Call, path: "/service-b/v1/hello/"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := startEcho(t, "service-b")
			sc := startSidecar(t, map[string]routing.ServiceConfig{"service-b": instances(b.Addr)})

			res, err := tc.call(sc.Addr, "service-b", tc.path)
			if err != nil {
				t.Fatalf("call service-b: %v", err)
			}

			if res.StatusCode != http.StatusOK {
				t.Errorf("status = %d, want 200", res.StatusCode)
			}
			if code := res.Header.Get("X-Sidecar-Error"); code != "" {
				t.Errorf("X-Sidecar-Error = %q, want empty (the sidecar should not have generated this response)", code)
			}

			got, err := ReadReceived(res)
			if err != nil {
				t.Fatal(err)
			}
			if got.Service != "service-b" {
				t.Errorf("reached service %q, want service-b", got.Service)
			}
			if got.Path != tc.path {
				t.Errorf("service-b saw path %q, want %q", got.Path, tc.path)
			}
			if got.XForwardedFor == "" {
				t.Error("service-b saw no X-Forwarded-For; the sidecar should stamp it")
			}
			if b.Requests() != 1 {
				t.Errorf("service-b served %d requests, want 1", b.Requests())
			}
		})
	}
}

// An instance address is `host:port` with no scheme, but the host itself is
// whatever the machine can reach: a DNS name or an IPv6 literal is not a lesser
// form of an IPv4 address. Nothing in the sidecar resolves names itself — that
// stays with the dialler — so this is really a test that it does not interfere.
func TestInstanceAddressFamilies(t *testing.T) {
	tests := []struct {
		name    string
		network string
		bind    string
	}{
		{name: "ipv4", network: "tcp4", bind: "127.0.0.1:0"},
		{name: "ipv6", network: "tcp6", bind: "[::1]:0"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := StartEchoOn("service-b", tc.network, tc.bind)
			if b == nil {
				t.Skipf("this host cannot listen on %s", tc.bind)
			}
			t.Cleanup(b.Close)

			_, port, err := net.SplitHostPort(b.Addr)
			if err != nil {
				t.Fatalf("split %q: %v", b.Addr, err)
			}

			// The literal the listener reported, and a DNS name for the same
			// port — "localhost" resolves to both families.
			for _, instance := range []string{b.Addr, "localhost:" + port} {
				t.Run(instance, func(t *testing.T) {
					sc := startSidecar(t, map[string]routing.ServiceConfig{
						"service-b": instances(instance),
					})

					res, err := Call(sc.Addr, "service-b", "/v1/hello")
					if err != nil {
						t.Fatalf("call service-b at %s: %v", instance, err)
					}
					if res.StatusCode != http.StatusOK {
						t.Fatalf("status = %d, want 200", res.StatusCode)
					}
					if code := res.Header.Get("X-Sidecar-Error"); code != "" {
						t.Errorf("X-Sidecar-Error = %q, want empty", code)
					}

					got, err := ReadReceived(res)
					if err != nil {
						t.Fatal(err)
					}
					if got.Service != "service-b" {
						t.Errorf("reached service %q, want service-b", got.Service)
					}
				})
			}
		})
	}
}

func TestUnknownService(t *testing.T) {
	b := startEcho(t, "service-b")
	sc := startSidecar(t, map[string]routing.ServiceConfig{"service-b": instances(b.Addr)})

	res, err := Call(sc.Addr, "service-nope", "/v1/hello")
	if err != nil {
		t.Fatalf("call service-nope: %v", err)
	}

	if res.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", res.StatusCode)
	}
	if code := res.Header.Get("X-Sidecar-Error"); code != "no_route" {
		t.Errorf("X-Sidecar-Error = %q, want no_route", code)
	}

	got, err := ReadSidecarError(res)
	if err != nil {
		t.Fatal(err)
	}
	if got.Code != "no_route" {
		t.Errorf("body code = %q, want no_route", got.Code)
	}
	if b.Requests() != 0 {
		t.Errorf("service-b served %d requests, want 0", b.Requests())
	}
}

// The route exists, but nothing is listening on it.
func TestUpstreamDown(t *testing.T) {
	b := StartEcho("service-b")
	addr := b.Addr
	b.Close() // the address is now dead, but still in the routing table

	sc := startSidecar(t, map[string]routing.ServiceConfig{"service-b": instances(addr)})

	res, err := Call(sc.Addr, "service-b", "/v1/hello")
	if err != nil {
		t.Fatalf("call service-b: %v", err)
	}

	if res.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", res.StatusCode)
	}
	if code := res.Header.Get("X-Sidecar-Error"); code != "upstream_connect_failed" {
		t.Errorf("X-Sidecar-Error = %q, want upstream_connect_failed", code)
	}
}

func TestRoundRobin(t *testing.T) {
	b1 := startEcho(t, "service-b#1")
	b2 := startEcho(t, "service-b#2")
	sc := startSidecar(t, map[string]routing.ServiceConfig{"service-b": instances(b1.Addr, b2.Addr)})

	const calls = 4
	for i := range calls {
		res, err := Call(sc.Addr, "service-b", "/v1/hello")
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if res.StatusCode != http.StatusOK {
			t.Fatalf("call %d: status = %d, want 200", i, res.StatusCode)
		}
		res.Body.Close()
	}

	if b1.Requests() != calls/2 || b2.Requests() != calls/2 {
		t.Errorf("requests split %d/%d, want %d/%d", b1.Requests(), b2.Requests(), calls/2, calls/2)
	}
}

// The upstream takes far longer than the budget, so finishing quickly is itself
// part of the assertion.
func TestDeadlineExceeded(t *testing.T) {
	const (
		timeout = 50 * time.Millisecond
		delay   = 2 * time.Second
	)

	b := StartSlowEcho("service-b", delay)
	t.Cleanup(b.Close)
	sc := startSidecar(t, map[string]routing.ServiceConfig{
		"service-b": {Instances: []string{b.Addr}, Timeout: timeout},
	})

	start := time.Now()
	res, err := Call(sc.Addr, "service-b", "/v1/hello")
	if err != nil {
		t.Fatalf("call service-b: %v", err)
	}
	elapsed := time.Since(start)

	if res.StatusCode != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want 504", res.StatusCode)
	}
	if code := res.Header.Get("X-Sidecar-Error"); code != "deadline_exceeded" {
		t.Errorf("X-Sidecar-Error = %q, want deadline_exceeded", code)
	}
	if elapsed >= delay {
		t.Errorf("took %v, i.e. the sidecar waited for the upstream instead of enforcing its %v budget", elapsed, timeout)
	}

	got, err := ReadSidecarError(res)
	if err != nil {
		t.Fatal(err)
	}
	if got.Code != "deadline_exceeded" {
		t.Errorf("body code = %q, want deadline_exceeded", got.Code)
	}
}

func TestSlowButWithinBudget(t *testing.T) {
	b := StartSlowEcho("service-b", 50*time.Millisecond)
	t.Cleanup(b.Close)
	sc := startSidecar(t, map[string]routing.ServiceConfig{
		"service-b": {Instances: []string{b.Addr}, Timeout: 2 * time.Second},
	})

	res, err := Call(sc.Addr, "service-b", "/v1/hello")
	if err != nil {
		t.Fatalf("call service-b: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}

	got, err := ReadReceived(res)
	if err != nil {
		t.Fatal(err)
	}
	if got.Service != "service-b" {
		t.Errorf("reached service %q, want service-b", got.Service)
	}
}

// "Nowhere to send it" is 503, not the 404 of "no such service".
func TestNoInstances(t *testing.T) {
	sc := startSidecar(t, map[string]routing.ServiceConfig{"service-b": instances()})

	res, err := Call(sc.Addr, "service-b", "/v1/hello")
	if err != nil {
		t.Fatalf("call service-b: %v", err)
	}

	if res.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", res.StatusCode)
	}
	if code := res.Header.Get("X-Sidecar-Error"); code != "no_healthy_upstream" {
		t.Errorf("X-Sidecar-Error = %q, want no_healthy_upstream", code)
	}
}
