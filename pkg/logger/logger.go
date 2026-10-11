package logger

import (
	"io"
	"log/slog"
	"strings"
)

// New returns a slog.Logger using Handler.
func New(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(NewHandler(w, level))
}

// Anything unrecognized defaults to slog.LevelDebug.
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "sql":
		return LevelSQL
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelDebug
	}
}
