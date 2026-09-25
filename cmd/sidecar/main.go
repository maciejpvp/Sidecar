// Command sidecar runs one sidecar: it registers the local app with the
// control plane while the app is healthy, and proxies the app's outbound calls
// by the snapshots the control plane sends.
//
//	go run ./cmd/sidecar -config sidecar.yaml
//	SIDECAR_SERVICE=orders-svc SIDECAR_ADVERTISE=10.1.2.3:15000 \
//	    SIDECAR_CONTROL_PLANE=controlplane:15100 go run ./cmd/sidecar
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"sidecar/internal/config"
	"sidecar/internal/logging"
	"sidecar/internal/sidecar"
)

func main() {
	path := flag.String("config", "", "optional YAML file; the environment (SIDECAR_*) overrides it")
	flag.Parse()

	var level slog.LevelVar
	logger := logging.New(&level)
	slog.SetDefault(logger)

	cfg, err := config.LoadSidecar(*path, os.Getenv)
	if err != nil {
		slog.Error("config_rejected", "error", err)
		os.Exit(1)
	}
	level.Set(cfg.Log.Level)
	logger = logger.With("service", cfg.Service.Name)

	if err := run(cfg, logger); err != nil {
		logger.Error("sidecar_failed", "error", err)
		os.Exit(1)
	}
}

func run(cfg *config.Sidecar, log *slog.Logger) error {
	sc := sidecar.New(cfg, log)

	outboundLn, err := net.Listen("tcp", cfg.Listeners.Outbound)
	if err != nil {
		return err
	}
	adminLn, err := net.Listen("tcp", cfg.Listeners.Admin)
	if err != nil {
		outboundLn.Close()
		return err
	}
	outbound, admin := sc.OutboundServer(), sc.AdminServer()

	stop, cancelSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancelSignals()

	// Discovery gets its own context: it stops on a signal, but shutdown must
	// wait for the deregistration it does on the way out.
	discoveryCtx, stopDiscovery := context.WithCancel(context.Background())
	discoveryDone := make(chan struct{})
	go func() {
		sc.Run(discoveryCtx)
		close(discoveryDone)
	}()

	serveErr := make(chan error, 2)
	for _, s := range []struct {
		srv *http.Server
		ln  net.Listener
	}{{outbound, outboundLn}, {admin, adminLn}} {
		go func() {
			if err := s.srv.Serve(s.ln); !errors.Is(err, http.ErrServerClosed) {
				serveErr <- err
			}
		}()
	}
	log.Info("ready", "outbound", outboundLn.Addr().String(), "admin", adminLn.Addr().String(),
		"controlPlane", cfg.ControlPlane.Address, "advertise", cfg.Service.Advertise)

	var failure error
	select {
	case <-stop.Done():
	case failure = <-serveErr:
	}

	// Leave the mesh first, so callers stop picking this instance, then drain.
	// Outbound keeps serving throughout: the app may still be finishing
	// requests that call out, and in Kubernetes (a native sidecar) the app has
	// usually exited already by the time this process is signalled.
	log.Info("shutdown_started")
	stopDiscovery()
	<-discoveryDone

	ctx, cancel := context.WithTimeout(context.Background(), cfg.Shutdown.DrainTimeout)
	defer cancel()
	var wg sync.WaitGroup
	for _, srv := range []*http.Server{outbound, admin} {
		wg.Go(func() {
			if err := srv.Shutdown(ctx); err != nil {
				srv.Close()
			}
		})
	}
	wg.Wait()
	log.Info("shutdown_complete")
	return failure
}
