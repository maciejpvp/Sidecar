// Command controlplane is the mesh's registry: sidecars register with it and
// long-poll it for snapshots of every service's instances and policy.
//
//	go run ./cmd/controlplane -mesh mesh.yaml
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
	"syscall"
	"time"

	"sidecar/internal/config"
	"sidecar/internal/controlplane"
	"sidecar/internal/logging"
)

func main() {
	listen := flag.String("listen", ":15100", "address to serve the sidecar API on")
	meshPath := flag.String("mesh", "", "mesh policy file (YAML, hot-reloaded); empty runs every service on the defaults")
	warmup := flag.Duration("warmup", -1, "how long to refuse snapshots after start; -1 means one lease TTL, the safe value after a restart")
	flag.Parse()

	var level slog.LevelVar
	log := logging.New(&level)
	slog.SetDefault(log)

	mesh := config.Static(config.DefaultMesh())
	if *meshPath != "" {
		var err error
		if mesh, err = config.New(*meshPath, config.WithLogger(log)); err != nil {
			log.Error("config_rejected", "error", err)
			os.Exit(1)
		}
	}
	level.Set(mesh.Current().Log.Level)
	mesh.OnChange(func(_, next *config.Mesh) { level.Set(next.Log.Level) })

	if *warmup < 0 {
		*warmup = mesh.Current().Registry.LeaseTTL
	}

	if err := run(*listen, mesh, *warmup, log); err != nil {
		log.Error("controlplane_failed", "error", err)
		os.Exit(1)
	}
}

func run(listen string, mesh *config.Source, warmup time.Duration, log *slog.Logger) error {
	srv := controlplane.NewServer(mesh, controlplane.NewRegistry(nil), log, warmup)

	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go mesh.Watch(ctx)
	go srv.Run(ctx)

	httpSrv := &http.Server{Handler: srv, ReadHeaderTimeout: 5 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.Serve(ln) }()
	log.Info("ready", "addr", ln.Addr().String(), "warmup", warmup.String())

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		return err
	}

	// Heartbeats get a moment to finish; long-polls are then cut rather than
	// drained. Every sidecar reconnects, and keeps its table until the next
	// control plane has warmed up.
	log.Info("shutdown_started")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); errors.Is(err, context.DeadlineExceeded) {
		httpSrv.Close()
	}
	log.Info("shutdown_complete")
	return nil
}
