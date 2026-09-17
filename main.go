package main

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"sidecar/internal/logging"
	"sidecar/internal/proxy"
	"sidecar/internal/routing"
)

// Loopback only: on any other interface this is an open proxy into the mesh.
const outboundAddr = "127.0.0.1:15001"

func main() {
	var level slog.LevelVar
	level.Set(slog.LevelDebug)
	logger := logging.New(&level)
	slog.SetDefault(logger)

	table := routing.NewTable(map[string]routing.ServiceConfig{
		"service1": {Instances: []string{"https://www.youtube.com/"}},
		"billing": {
			Instances: []string{"http://10.0.0.1:8080", "http://10.0.0.2:8080", "http://10.0.0.3:8080"},
			Timeout:   2 * time.Second,
		},
	})

	p := proxy.New(table, logger)

	ln, err := net.Listen("tcp", outboundAddr)
	if err != nil {
		slog.Error("outbound listener failed to bind", "addr", outboundAddr, "error", err)
		os.Exit(1)
	}

	slog.Info("ready", "dir", "outbound", "addr", ln.Addr().String())

	// No ReadTimeout/WriteTimeout: they would cap the whole exchange, fighting
	// the per-service deadline and cutting off streaming responses.
	srv := &http.Server{
		Handler:           p,
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("outbound listener stopped", "addr", outboundAddr, "error", err)
		os.Exit(1)
	}
}
