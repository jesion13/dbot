package logger

import (
	"io"
	"log/slog"
)

// New returns a slog.Logger using Handler.
func New(w io.Writer) *slog.Logger {
	return slog.New(NewHandler(w))
}
