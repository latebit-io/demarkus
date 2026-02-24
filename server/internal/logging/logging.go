// Package logging provides structured logger creation for the Demarkus server.
package logging

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// New creates a *slog.Logger configured with the given format and level.
// format: "text" (default) or "json".
// level: "debug", "info" (default), "warn", "error".
// If w is nil, os.Stderr is used.
func New(format, level string, w io.Writer) *slog.Logger {
	if w == nil {
		w = os.Stderr
	}

	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: lvl}

	var handler slog.Handler
	switch strings.ToLower(format) {
	case "json":
		handler = slog.NewJSONHandler(w, opts)
	default:
		handler = slog.NewTextHandler(w, opts)
	}

	return slog.New(handler)
}
