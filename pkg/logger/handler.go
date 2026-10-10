package logger

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
)

const timeFormat = "15:04:05.000"

// LevelSQL is a verbosity level below slog.LevelDebug, intended for very
// noisy per-query logging (see pkg/db) that most Debug sessions don't want.
const LevelSQL = slog.LevelDebug - 4

// Handler is a slog.Handler that renders:
//
//	15:04:05.000 LEVEL pkg/file.go:123 message
//	   Key=value
//	   Struct.Field=value
type Handler struct {
	out   io.Writer
	level slog.Level
}

// NewHandler returns a Handler that logs records at level and above.
func NewHandler(out io.Writer, level slog.Level) *Handler {
	return &Handler{out: out, level: level}
}

func (h *Handler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

// ANSI color codes, matching fr-str/log's old TextHandler palette.
const (
	colorReset   = "\x1b[0m"
	colorGrey    = "\x1b[90m"
	colorRed     = "\x1b[31;1m"
	colorYellow  = "\x1b[33;1m"
	colorBlue    = "\x1b[34;1m"
	colorMagenta = "\x1b[35;1m"
	colorWhite   = "\x1b[37;1m"
)

// maxLineWidth is the limit (excluding ANSI color codes) under which a log
// record is rendered on a single line instead of one line per attr.
const maxLineWidth = 120

func (h *Handler) Handle(_ context.Context, r slog.Record) error {
	timeStr := r.Time.Format(timeFormat)
	levelStr := fmt.Sprintf("%-5s", levelString(r.Level))
	callerStr := caller(r.PC)

	var plain strings.Builder
	fmt.Fprintf(&plain, "%s %s %s %s", timeStr, levelStr, callerStr, r.Message)

	lines := make([]string, 0, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		lines = appendAttr(lines, "", a)
		return true
	})

	width := plain.Len()
	for _, line := range lines {
		width += 1 + len(line)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s %s%s%s %s%s%s %s",
		timeStr,
		levelColor(r.Level), levelStr, colorReset,
		colorGrey, callerStr, colorReset,
		r.Message)

	if width <= maxLineWidth {
		for _, line := range lines {
			b.WriteByte(' ')
			b.WriteString(line)
		}
		b.WriteByte('\n')
	} else {
		b.WriteByte('\n')
		for _, line := range lines {
			b.WriteString("    ")
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}

	_, err := io.WriteString(h.out, b.String())
	return err
}

func (h *Handler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *Handler) WithGroup(string) slog.Handler      { return h }

func levelString(l slog.Level) string {
	if l == LevelSQL {
		return "SQL"
	}
	return l.String()
}

func levelColor(l slog.Level) string {
	switch {
	case l == LevelSQL:
		return colorWhite
	case l < slog.LevelInfo:
		return colorMagenta
	case l < slog.LevelWarn:
		return colorBlue
	case l < slog.LevelError:
		return colorYellow
	default:
		return colorRed
	}
}

func caller(pc uintptr) string {
	if pc == 0 {
		return ""
	}
	frame, _ := runtime.CallersFrames([]uintptr{pc}).Next()
	dir := filepath.Base(filepath.Dir(frame.File))
	return fmt.Sprintf("%s/%s:%d", dir, filepath.Base(frame.File), frame.Line)
}

// appendAttr flattens a into one or more "key=value" lines, recursing into
// groups and struct-valued Any attrs with a dotted "Parent.Child" key.
func appendAttr(lines []string, prefix string, a slog.Attr) []string {
	key := a.Key
	if prefix != "" {
		key = prefix + "." + key
	}

	if a.Value.Kind() == slog.KindGroup {
		for _, ga := range a.Value.Group() {
			lines = appendAttr(lines, key, ga)
		}
		return lines
	}

	return appendAny(lines, key, a.Value.Any())
}

// appendAny renders v as a single "key=value" line, or - if v is a struct
// (or pointer to one) that isn't a leaf type - flattens its exported fields
// recursively with a dotted "key.Field" prefix.
func appendAny(lines []string, key string, v any) []string {
	if v == nil {
		return append(lines, fmt.Sprintf("%s=<nil>", key))
	}

	// Checked before any pointer deref/reflection, so error/Stringer
	// implementations with a pointer receiver are respected.
	if s, ok := asLeaf(v); ok {
		return append(lines, fmt.Sprintf("%s=%s", key, s))
	}

	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return append(lines, fmt.Sprintf("%s=<nil>", key))
		}
		rv = rv.Elem()
	}

	if rv.Kind() == reflect.Struct {
		return appendStruct(lines, key, rv)
	}

	return append(lines, fmt.Sprintf("%s=%v", key, v))
}

func appendStruct(lines []string, prefix string, v reflect.Value) []string {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		key := prefix + "." + field.Name
		lines = appendAny(lines, key, v.Field(i).Interface())
	}
	return lines
}

// asLeaf reports whether v should be printed as a single scalar value
// rather than reflected into.
func asLeaf(v any) (string, bool) {
	switch val := v.(type) {
	case string:
		return fmt.Sprintf("%q", val), true
	case error:
		return fmt.Sprintf("%q", val.Error()), true
	case fmt.Stringer:
		return fmt.Sprintf("%q", val.String()), true
	}
	return "", false
}
