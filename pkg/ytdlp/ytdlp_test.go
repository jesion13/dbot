package ytdlp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSpecialLinks(t *testing.T) {
	t.Run("non-jbzd links pass through unchanged", func(t *testing.T) {
		link := "https://www.youtube.com/watch?v=abc123"
		out, err := parseSpecialLinks(link)
		require.NoError(t, err)
		assert.Equal(t, link, out)
	})

	t.Run("invalid url", func(t *testing.T) {
		_, err := parseSpecialLinks("http://[::1]:namedport")
		assert.Error(t, err)
	})
}
