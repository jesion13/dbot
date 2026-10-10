package logger

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandler_Handle(t *testing.T) {
	t.Run("plain message with no attrs", func(t *testing.T) {
		var buf bytes.Buffer
		h := NewHandler(&buf, slog.LevelDebug)
		r := slog.NewRecord(time.Now(), slog.LevelInfo, "hello", 0)
		require.NoError(t, h.Handle(context.Background(), r))
		assert.Contains(t, buf.String(), "hello")
		assert.Contains(t, buf.String(), "INFO")
	})

	t.Run("short attrs render on one line", func(t *testing.T) {
		var buf bytes.Buffer
		h := NewHandler(&buf, slog.LevelDebug)
		r := slog.NewRecord(time.Now(), slog.LevelInfo, "msg", 0)
		r.AddAttrs(slog.String("key", "value"))
		require.NoError(t, h.Handle(context.Background(), r))
		out := buf.String()
		assert.Equal(t, 1, strings.Count(out, "\n"))
		assert.Contains(t, out, `key="value"`)
	})

	t.Run("SQL level renders as SQL", func(t *testing.T) {
		var buf bytes.Buffer
		h := NewHandler(&buf, LevelSQL)
		r := slog.NewRecord(time.Now(), LevelSQL, "query", 0)
		require.NoError(t, h.Handle(context.Background(), r))
		assert.Contains(t, buf.String(), "SQL")
	})
}

func TestHandler_Enabled(t *testing.T) {
	h := NewHandler(&bytes.Buffer{}, slog.LevelInfo)
	assert.False(t, h.Enabled(context.Background(), slog.LevelDebug))
	assert.True(t, h.Enabled(context.Background(), slog.LevelInfo))
	assert.True(t, h.Enabled(context.Background(), slog.LevelError))
}

func TestAppendAny(t *testing.T) {
	t.Run("string is quoted", func(t *testing.T) {
		lines := appendAny(nil, "k", "v")
		assert.Equal(t, []string{`k="v"`}, lines)
	})

	t.Run("error uses Error() before reflection", func(t *testing.T) {
		lines := appendAny(nil, "err", errors.New("boom"))
		assert.Equal(t, []string{`err="boom"`}, lines)
	})

	t.Run("stringer uses String() before reflection", func(t *testing.T) {
		lines := appendAny(nil, "s", stringerStruct{Field: "ignored"})
		assert.Equal(t, []string{`s="stringer-value"`}, lines)
	})

	t.Run("nil value", func(t *testing.T) {
		lines := appendAny(nil, "k", nil)
		assert.Equal(t, []string{"k=<nil>"}, lines)
	})

	t.Run("nil pointer", func(t *testing.T) {
		var p *plainStruct
		lines := appendAny(nil, "k", p)
		assert.Equal(t, []string{"k=<nil>"}, lines)
	})

	t.Run("struct flattens exported fields recursively", func(t *testing.T) {
		v := plainStruct{Name: "n", Nested: nestedStruct{Age: 5}}
		lines := appendAny(nil, "p", v)
		assert.Equal(t, []string{`p.Name="n"`, "p.Nested.Age=5"}, lines)
	})

	t.Run("pointer to struct is dereferenced", func(t *testing.T) {
		v := &plainStruct{Name: "n", Nested: nestedStruct{Age: 5}}
		lines := appendAny(nil, "p", v)
		assert.Equal(t, []string{`p.Name="n"`, "p.Nested.Age=5"}, lines)
	})

	t.Run("unexported fields skipped", func(t *testing.T) {
		v := structWithPrivate{Public: "a", private: "b"}
		lines := appendAny(nil, "p", v)
		assert.Equal(t, []string{`p.Public="a"`}, lines)
	})

	t.Run("non-struct scalar", func(t *testing.T) {
		lines := appendAny(nil, "n", 42)
		assert.Equal(t, []string{"n=42"}, lines)
	})
}

func TestAppendAttr_Group(t *testing.T) {
	g := slog.Group("grp", slog.String("a", "1"), slog.Int("b", 2))
	lines := appendAttr(nil, "", g)
	assert.Equal(t, []string{`grp.a="1"`, "grp.b=2"}, lines)
}

func TestLevelString(t *testing.T) {
	assert.Equal(t, "SQL", levelString(LevelSQL))
	assert.Equal(t, "INFO", levelString(slog.LevelInfo))
}

type plainStruct struct {
	Name   string
	Nested nestedStruct
}

type nestedStruct struct {
	Age int
}

type structWithPrivate struct {
	Public  string
	private string
}

type stringerStruct struct {
	Field string
}

func (stringerStruct) String() string { return "stringer-value" }
