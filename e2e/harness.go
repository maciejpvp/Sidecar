// Package e2e wires real sidecars and toy services together so a request can be
// followed end to end. Shared by the tests here and the example in ./demo.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"time"

	"sidecar/internal/config"
	"sidecar/internal/proxy"
	"sidecar/internal/routing"
	"sidecar/internal/sidecar"
)

// Received is the view from the far side of the sidecar hop.
type Received struct {
	Service         string `json:"service"`
	Method          string `json:"method"`
	Path            string `json:"path"`
	Host            string `json:"host"`
	XForwardedFor   string `json:"xForwardedFor"`
	XForwardedHost  string `json:"xForwardedHost"`
	XForwardedProto string `json:"xForwardedProto"`
}

// Echo is a toy service: it answers every request by describing it.
type Echo struct {
	Name string
	// Addr is host:port, the form a routing table stores an instance in.
	Addr string

	requests atomic.Uint64
	srv      *httptest.Server
}

func StartEcho(name string) *Echo { return newEcho(name, 0, nil) }

func StartSlowEcho(name string, delay time.Duration) *Echo { return newEcho(name, delay, nil) }

// StartEchoOn is StartEcho on a listener of the caller's choosing, so a test can
// put an instance on IPv6. It returns nil when the host cannot bind there at
// all, which is the caller's cue to skip.
func StartEchoOn(name, network, bind string) *Echo {
	ln, err := net.Listen(network, bind)
	if err != nil {
		return nil
	}
	return newEcho(name, 0, ln)
}

// A nil listener means "wherever httptest puts it", i.e. IPv4 loopback.
func newEcho(name string, delay time.Duration, ln net.Listener) *Echo {
	e := &Echo{Name: name}
	e.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.requests.Add(1)
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(Received{
			Service:         name,
			Method:          r.Method,
			Path:            r.URL.Path,
			Host:            r.Host,
			XForwardedFor:   r.Header.Get("X-Forwarded-For"),
			XForwardedHost:  r.Header.Get("X-Forwarded-Host"),
			XForwardedProto: r.Header.Get("X-Forwarded-Proto"),
		})
	}))
	if ln != nil {
		e.srv.Listener.Close()
		e.srv.Listener = ln
	}
	e.srv.Start()
	e.Addr = e.srv.Listener.Addr().String()
	return e
}

func (e *Echo) Requests() uint64 { return e.requests.Load() }

func (e *Echo) Close() { e.srv.Close() }

type Sidecar struct {
	Addr string

	srv *http.Server
}

// StartSidecar routes to services built with config.NewService.
func StartSidecar(addr string, services []config.Service, log *slog.Logger) (*Sidecar, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("bind outbound listener on %s: %w", addr, err)
	}

	s := &Sidecar{
		Addr: ln.Addr().String(),
		srv:  &http.Server{Handler: proxy.New(routing.NewTable(services), log)},
	}
	go s.srv.Serve(ln)
	return s, nil
}

// StartSidecarFromConfig starts a sidecar from a config file with main's wiring,
// listening on addr rather than listeners.outbound so tests can use any port.
func StartSidecarFromConfig(addr, path string, log *slog.Logger) (*Sidecar, *config.Source, error) {
	cfg, err := config.New(path, config.WithLogger(log))
	if err != nil {
		return nil, nil, err
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("bind outbound listener on %s: %w", addr, err)
	}
	s := &Sidecar{Addr: ln.Addr().String(), srv: sidecar.Server(cfg, sidecar.Handler(cfg, nil, log))}
	go s.srv.Serve(ln)
	return s, cfg, nil
}

func (s *Sidecar) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.srv.Shutdown(ctx)
}

// Call is how an app addresses a service: connect to the local sidecar, name
// the service in Host. The path stays the upstream's own.
func Call(sidecarAddr, service, path string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, "http://"+sidecarAddr+path, nil)
	if err != nil {
		return nil, err
	}
	req.Host = service
	return http.DefaultClient.Do(req)
}

// CallViaProxy is the same, addressed the way http_proxy does it: the name
// arrives in the request line rather than the Host header.
func CallViaProxy(sidecarAddr, service, path string) (*http.Response, error) {
	sidecarURL, err := url.Parse("http://" + sidecarAddr)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(sidecarURL)}}
	return client.Get("http://" + service + path)
}

func ReadReceived(res *http.Response) (Received, error) {
	defer res.Body.Close()
	var got Received
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		return got, fmt.Errorf("decode echo response: %w", err)
	}
	return got, nil
}

// SidecarError is the body for sidecar-generated failures (DESIGN §7).
type SidecarError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func ReadSidecarError(res *http.Response) (SidecarError, error) {
	defer res.Body.Close()
	var got SidecarError
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		return got, fmt.Errorf("decode sidecar error: %w", err)
	}
	return got, nil
}
