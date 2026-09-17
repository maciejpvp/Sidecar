package logging

import (
	"log/slog"
	"os"
)

func New(level *slog.LevelVar) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level:     level,
		AddSource: true,
	}))
}
