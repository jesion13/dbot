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

// Handler is a slog.Handler that renders:
//
//	15:04:05.000 LEVEL pkg/file.go:123 message {
//	   Key=value
//	   Struct.Field=value
//	}
type Handler struct {
	out io.Writer
}

func NewHandler(out io.Writer) *Handler {
	return &Handler{out: out}
}

func (h *Handler) Enabled(context.Context, slog.Level) bool { return true }

func (h *Handler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %-5s %s %s", r.Time.Format(timeFormat), r.Level.String(), caller(r.PC), r.Message)

	lines := make([]string, 0, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		lines = appendAttr(lines, "", a)
		return true
	})

	if len(lines) == 0 {
		b.WriteByte('\n')
		_, err := io.WriteString(h.out, b.String())
		return err
	}

	b.WriteString(" {\n")
	for _, line := range lines {
		b.WriteString("   ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteString("}\n")

	_, err := io.WriteString(h.out, b.String())
	return err
}

func (h *Handler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *Handler) WithGroup(string) slog.Handler       { return h }

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
