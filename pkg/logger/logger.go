package logger

import (
	"io"
	"log/slog"
	"strings"
)

// New returns a slog.Logger using Handler, logging at level and above.
func New(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(NewHandler(w, level))
}

// ParseLevel maps a config string to a Level, defaulting to slog.LevelDebug
// for anything unrecognized.
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
