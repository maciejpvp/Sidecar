package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"

	"sidecar/internal/config"
	"sidecar/internal/logging"
	"sidecar/internal/sidecar"
)

func main() {
	path := flag.String("config", config.DefaultPath, "path to the YAML configuration file")
	flag.Parse()

	var level slog.LevelVar
	logger := logging.New(&level)
	slog.SetDefault(logger)

	cfg, err := config.New(*path, config.WithLogger(logger))
	if err != nil {
		slog.Error("config_rejected", "error", err)
		os.Exit(1)
	}

	handler := sidecar.Handler(cfg, &level, logger)
	// Background until shutdown handling exists (TODO P0).
	go cfg.Watch(context.Background())

	addr := cfg.Current().Listeners.Outbound
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		slog.Error("outbound listener failed to bind", "addr", addr, "error", err)
		os.Exit(1)
	}

	slog.Info("ready", "dir", "outbound", "addr", ln.Addr().String())

	if err := sidecar.Server(cfg, handler).Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("outbound listener stopped", "addr", addr, "error", err)
		os.Exit(1)
	}
}
