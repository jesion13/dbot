package types

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAliases_Scan(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		var a Aliases
		err := a.Scan("foo,bar,baz")
		require.NoError(t, err)
		assert.Equal(t, Aliases{"foo", "bar", "baz"}, a)
	})

	t.Run("fail", func(t *testing.T) {
		var a Aliases
		err := a.Scan(123)
		assert.Error(t, err)
	})
}

func TestAliases_Value(t *testing.T) {
	a := Aliases{"foo", "bar", "baz"}
	v, err := a.Value()
	require.NoError(t, err)
	assert.Equal(t, "foo,bar,baz", v)
}
