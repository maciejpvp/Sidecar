package main

import (
	"log/slog"
	"net/http"
	"sidecar/internal/logging"
	"sidecar/internal/proxy"
	"sidecar/internal/routing"
)

func main() {
	var level slog.LevelVar
	level.Set(slog.LevelDebug)
	logger := logging.New(&level)
	slog.SetDefault(logger)

	slog.Info("ready", "addr", ":8080")
	table := routing.NewTable(map[string][]string{
		"service1": {"https://www.youtube.com/"},
		"billing":  {"http://10.0.0.1:8080", "http://10.0.0.2:8080", "http://10.0.0.3:8080"},
	})

	p := proxy.New(table, logger)

	http.ListenAndServe(":8080", p)
}
