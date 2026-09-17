package e2e

import (
	"io"
	"log/slog"
	"net/http"
	"testing"
)

// quietLogger keeps the sidecar's JSON logs out of test output. Swap the
// writer for os.Stdout when a test is misbehaving and you want to watch it.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// startSidecar starts an ephemeral-port sidecar and stops it when the test ends.
func startSidecar(t *testing.T, routes map[string][]string) *Sidecar {
	t.Helper()
	s, err := StartSidecar("127.0.0.1:0", routes, quietLogger())
	if err != nil {
		t.Fatalf("start sidecar: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// startEcho starts a toy service and stops it when the test ends.
func startEcho(t *testing.T, name string) *Echo {
	t.Helper()
	e := StartEcho(name)
	t.Cleanup(e.Close)
	return e
}

// TestRequestFromAToB is the whole point of this package: app A calls service B
// by name through its sidecar, and B receives the request unchanged.
func TestRequestFromAToB(t *testing.T) {
	tests := []struct {
		name string
		call func(sidecarAddr, service, path string) (*http.Response, error)
		path string
	}{
		// The two forms serviceName accepts: a per-request Host header, and
		// the absolute request URI a configured HTTP proxy receives.
		{name: "host header", call: Call, path: "/v1/hello"},
		{name: "proxy style", call: CallViaProxy, path: "/v1/hello"},
		// Routing by Host means the sidecar owns no part of the app's URL
		// space, so a path whose first segment is itself a service name is
		// still just a path.
		{name: "path kept verbatim", call: Call, path: "/service-b/v1/hello/"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := startEcho(t, "service-b")
			sc := startSidecar(t, map[string][]string{"service-b": {b.URL}})

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

// TestUnknownService covers the no_route arm of the error model (DESIGN §7).
func TestUnknownService(t *testing.T) {
	b := startEcho(t, "service-b")
	sc := startSidecar(t, map[string][]string{"service-b": {b.URL}})

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

// TestUpstreamDown covers the ReverseProxy ErrorHandler: the route exists but
// nothing is listening on it.
func TestUpstreamDown(t *testing.T) {
	b := StartEcho("service-b")
	addr := b.URL
	b.Close() // the address is now dead, but still in the routing table

	sc := startSidecar(t, map[string][]string{"service-b": {addr}})

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

// TestRoundRobin checks that two instances of one service each get half the
// requests.
func TestRoundRobin(t *testing.T) {
	b1 := startEcho(t, "service-b#1")
	b2 := startEcho(t, "service-b#2")
	sc := startSidecar(t, map[string][]string{"service-b": {b1.URL, b2.URL}})

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
