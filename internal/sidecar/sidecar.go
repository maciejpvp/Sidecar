// Package sidecar wires config, discovery, routing and proxy together. main
// and the e2e harness both use it, so the wiring the tests exercise is the one
// that ships.
package sidecar

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"sidecar/internal/config"
	"sidecar/internal/discovery"
	"sidecar/internal/proxy"
	"sidecar/internal/routing"
)

// Sidecar is one running sidecar's parts.
type Sidecar struct {
	Config    *config.Sidecar
	Routes    *routing.Store
	Proxy     *proxy.Handler
	Watcher   *discovery.Watcher
	Registrar *discovery.Registrar
}

// New builds a sidecar that routes by the control plane's snapshots and
// registers itself while its app is healthy. Nothing runs until Run.
func New(cfg *config.Sidecar, log *slog.Logger) *Sidecar {
	routes := routing.NewStore(nil) // not ready until the first snapshot
	p := proxy.New(routes, log)
	client := discovery.NewClient(cfg.ControlPlane.Address)

	return &Sidecar{
		Config: cfg,
		Routes: routes,
		Proxy:  p,
		Watcher: discovery.NewWatcher(client, log, func(services []config.Service) {
			// Swap first, so no request opens a connection to a removed
			// instance after the pool is cleared.
			routes.Swap(routing.NewTable(services))
			p.CloseIdleConnections()
		}),
		Registrar: discovery.NewRegistrar(client, log, cfg.Service.Name, cfg.Service.Advertise,
			discovery.HTTPHealth(cfg.App.Address, cfg.App.HealthPath)),
	}
}

// Run watches for snapshots and keeps this instance registered until ctx is
// done. It returns once the instance has deregistered, which is the signal
// that callers are no longer being sent here and draining can start.
func (s *Sidecar) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Go(func() { s.Watcher.Run(ctx) })
	wg.Go(func() { s.Registrar.Run(ctx) })
	wg.Wait()
}

// OutboundServer is the app's gateway into the mesh; its settings are restart-only.
func (s *Sidecar) OutboundServer() *http.Server {
	// No ReadTimeout/WriteTimeout: they would cap the whole exchange, fighting
	// the per-service deadline and cutting off streaming responses.
	return &http.Server{
		Handler:           s.Proxy,
		ReadHeaderTimeout: 10 * time.Second,
		MaxHeaderBytes:    s.Config.Limits.MaxHeaderBytes,
	}
}

// AdminServer serves the probes the kubelet uses and a view of the routes:
//
//	/healthz   the process is up (liveness)
//	/readyz    a snapshot has been applied (readiness)
//	/snapshot  the snapshot in effect, as received
func (s *Sidecar) AdminServer() *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		// Readiness is about routing, not registration: a pod whose app is
		// still starting is not registered, and that is the app's readiness
		// to report, not ours.
		if s.Watcher.Current() == nil {
			http.Error(w, "no snapshot from the control plane yet", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /snapshot", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.Encode(struct {
			Registered bool `json:"registered"`
			Snapshot   any  `json:"snapshot"`
		}{s.Registrar.Registered(), s.Watcher.Current()})
	})
	return &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
}
