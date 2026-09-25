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
	"sidecar/internal/controlplane"
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

	requests  atomic.Uint64
	unhealthy atomic.Bool
	srv       *httptest.Server
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
		// The sidecar's health probe, answered apart so it does not count as
		// traffic: tests compare request counts across instances.
		if r.URL.Path == HealthPath {
			if e.unhealthy.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
			return
		}
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

// HealthPath is where an Echo answers its sidecar's health probe.
const HealthPath = "/healthz"

func (e *Echo) Requests() uint64 { return e.requests.Load() }

// SetHealthy makes the health probe pass or fail; traffic is served either way,
// the way an app that is shutting down still finishes what it was sent.
func (e *Echo) SetHealthy(ok bool) { e.unhealthy.Store(!ok) }

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

func (s *Sidecar) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.srv.Shutdown(ctx)
}

// ControlPlane is a control plane on an ephemeral loopback port.
type ControlPlane struct {
	Addr   string
	Server *controlplane.Server
	Mesh   *config.Source

	srv    *http.Server
	cancel context.CancelFunc
}

// StartControlPlane serves mesh with the given warmup; tests pass 0 unless
// warmup is what they are testing.
func StartControlPlane(mesh *config.Source, warmup time.Duration, log *slog.Logger) (*ControlPlane, error) {
	return StartControlPlaneOn("127.0.0.1:0", mesh, warmup, log)
}

// StartControlPlaneOn is StartControlPlane on a fixed address, so a test can
// restart one where its sidecars expect it.
func StartControlPlaneOn(addr string, mesh *config.Source, warmup time.Duration, log *slog.Logger) (*ControlPlane, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("bind control plane: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	server := controlplane.NewServer(mesh, controlplane.NewRegistry(nil), log, warmup)
	go server.Run(ctx)

	cp := &ControlPlane{Addr: ln.Addr().String(), Server: server, Mesh: mesh, srv: &http.Server{Handler: server}, cancel: cancel}
	go cp.srv.Serve(ln)
	return cp, nil
}

func (cp *ControlPlane) Close() {
	cp.cancel()
	cp.srv.Close() // long-polls are cut, as a crashed control plane would
}

// MeshSidecar is a complete sidecar as cmd/sidecar runs it — registrar,
// snapshot watcher, outbound proxy, admin endpoints — on ephemeral ports.
type MeshSidecar struct {
	Addr  string // outbound
	Admin string
	*sidecar.Sidecar

	outbound, admin *http.Server
	cancel          context.CancelFunc
	done            chan struct{}
}

// StartMeshSidecar runs a sidecar for app, registered as service. There is no
// inbound listener yet (docs/TODO.md), so the instance it advertises is the
// app itself, as the Kubernetes manifests in deploy/ do for now.
func StartMeshSidecar(controlPlaneAddr, service string, app *Echo, log *slog.Logger) (*MeshSidecar, error) {
	env := map[string]string{
		config.Env.Service:       service,
		config.Env.Advertise:     app.Addr,
		config.Env.ControlPlane:  controlPlaneAddr,
		config.Env.AppAddress:    app.Addr,
		config.Env.AppHealthPath: HealthPath,
	}
	cfg, err := config.LoadSidecar("", func(k string) string { return env[k] })
	if err != nil {
		return nil, err
	}

	sc := sidecar.New(cfg, log)
	// Fast enough that tests do not wait on the probe loop.
	sc.Registrar.Probe = 20 * time.Millisecond

	outLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("bind outbound listener: %w", err)
	}
	adminLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		outLn.Close()
		return nil, fmt.Errorf("bind admin listener: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	m := &MeshSidecar{
		Addr:     outLn.Addr().String(),
		Admin:    adminLn.Addr().String(),
		Sidecar:  sc,
		outbound: sc.OutboundServer(),
		admin:    sc.AdminServer(),
		cancel:   cancel,
		done:     make(chan struct{}),
	}
	go m.outbound.Serve(outLn)
	go m.admin.Serve(adminLn)
	go func() {
		sc.Run(ctx)
		close(m.done)
	}()
	return m, nil
}

// Close shuts down in cmd/sidecar's order: deregister, then drain. Safe to
// call twice.
func (m *MeshSidecar) Close() {
	m.cancel()
	<-m.done
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m.outbound.Shutdown(ctx)
	m.admin.Shutdown(ctx)
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
