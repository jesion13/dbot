package dbot

import (
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// checkSplit asserts the two invariants every chunked message must satisfy: each
// chunk fits the limit and is valid UTF-8, and concatenating them reproduces the
// input byte for byte.
func checkSplit(t *testing.T, original string, limit int) []string {
	t.Helper()
	chunks := splitMessages(original, limit)
	if original == "" {
		require.Empty(t, chunks)
		return chunks
	}
	for i, c := range chunks {
		assert.LessOrEqual(t, len(c), limit, "chunk %d is over the limit", i)
		assert.True(t, utf8.ValidString(c), "chunk %d is not valid utf-8", i)
	}
	assert.Equal(t, original, strings.Join(chunks, ""), "chunks must concatenate back to the input")
	return chunks
}

func TestSplitMessagesUnicode(t *testing.T) {
	t.Run("polish characters around chunk boundaries", func(t *testing.T) {
		// each of these is two bytes in UTF-8, so a naive byte-boundary cut lands
		// inside them exactly when the limit is odd relative to the prefix
		line := strings.Repeat("ąęłźż", 40)
		checkSplit(t, line, 7)
		checkSplit(t, line, 8)
		checkSplit(t, line, 9)
		checkSplit(t, line, 199)
	})

	t.Run("a title cut in the middle of a multibyte rune", func(t *testing.T) {
		// the boundary lands between the two bytes of "ż"
		original := strings.Repeat("a", 8) + "ż" + strings.Repeat("b", 40)
		chunks := checkSplit(t, original, 9)
		require.Greater(t, len(chunks), 1)
		assert.Equal(t, original[:8], chunks[0])
		assert.Equal(t, original[8:], strings.Join(chunks[1:], ""))
	})

	t.Run("other two-byte scripts", func(t *testing.T) {
		checkSplit(t, strings.Repeat("日本語のタイトル", 30), 11)
		checkSplit(t, strings.Repeat("안녕하세요", 40), 13)
		checkSplit(t, strings.Repeat("привет", 40), 14)
	})

	t.Run("three-byte characters", func(t *testing.T) {
		checkSplit(t, strings.Repeat("漢字", 50), 5)
		checkSplit(t, strings.Repeat("→★", 60), 7)
	})

	t.Run("four-byte characters", func(t *testing.T) {
		// emoji outside the BMP: 4 bytes each, so every limit can land mid-rune
		line := strings.Repeat("🎵🎶", 50)
		for _, limit := range []int{5, 6, 7, 8, 9, 11, 13, 199} {
			checkSplit(t, line, limit)
		}
	})

	t.Run("one very long unicode line spanning many chunks", func(t *testing.T) {
		original := strings.Repeat("🎼ódmęszanka-ąęłźż", 200)
		chunks := checkSplit(t, original, 1900)
		assert.Greater(t, len(chunks), 2)
	})

	t.Run("mixed newlines ascii and unicode", func(t *testing.T) {
		original := strings.Join([]string{
			"**Now:** Żółw 🐢",
			"",
			"**Up next:**",
			"2. Łąka w śniegu — " + strings.Repeat("🎵", 30),
			"3. ascii only",
			"4. " + strings.Repeat("ę", 100),
		}, "\n")
		checkSplit(t, original, 64)
		checkSplit(t, original, maxQueueMsgLen)
	})

	t.Run("a newline exactly at the limit", func(t *testing.T) {
		checkSplit(t, strings.Repeat("a", 9)+"\n"+strings.Repeat("b", 9), 10)
	})

	t.Run("consecutive newlines are preserved", func(t *testing.T) {
		original := "a\n\n\nb\n\n"
		chunks := checkSplit(t, original, 3)
		assert.Equal(t, original, strings.Join(chunks, ""))
	})

	t.Run("a real long queue keeps every title and position", func(t *testing.T) {
		var lines []string
		for i := range 300 {
			lines = append(lines, strconv.Itoa(i+2)+". Tytuł utworu numer "+strconv.Itoa(i)+" 🎵")
		}
		original := "**Now:** Aktualny\n**Up next:**\n" + strings.Join(lines, "\n")
		chunks := checkSplit(t, original, maxQueueMsgLen)

		joined := strings.Join(chunks, "\n")
		for i := range 300 {
			assert.Contains(t, joined, strconv.Itoa(i+2)+". Tytuł utworu numer "+strconv.Itoa(i)+" 🎵",
				"entry %d must survive the split", i)
		}
	})
}
