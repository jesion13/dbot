package dbot

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"dbot/pkg/store"

	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

const testGID = "492318912881491981"

func openTestStore(t *testing.T) *store.Queries {
	t.Helper()
	path, err := filepath.Abs("../../test.db")
	require.NoError(t, err)
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return store.New(db)
}

func TestFindSound(t *testing.T) {
	db := openTestStore(t)
	srq.s = map[string][]store.Sound{} // reset cache between runs

	tests := []struct {
		name      string
		input     string
		wantAlias string
	}{
		{"exact", "honk", "honk"},
		{"prefix", "hej", "hejnalmariacki"},
		{"typo", "honkk", "honk"},
		{"multi-alias contains", "cymbal", "cymbal"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sounds, err := findSound(db, tt.input, testGID)
			require.NoError(t, err)
			require.Len(t, sounds, 1)
			require.Contains(t, sounds[0].Aliases, tt.wantAlias)
		})
	}

	t.Run("no match", func(t *testing.T) {
		_, err := findSound(db, "asdfghjkl", testGID)
		require.True(t, errors.Is(err, ErrSoundNotFound))
	})

	t.Run("short input below prefix minimum", func(t *testing.T) {
		_, err := findSound(db, "ho", testGID)
		require.True(t, errors.Is(err, ErrSoundNotFound))
	})
}
