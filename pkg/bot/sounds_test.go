package dbot

import (
	"context"
	"errors"
	"strings"
	"testing"

	"dbot/pkg/config"
	"dbot/pkg/db"
	"dbot/pkg/db/types"
	"dbot/pkg/store"

	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

const testGID = "492318912881491981"

// openTestStore runs real migrations into a fresh temp DB and seeds the
// handful of sounds/aliases the matching tests below expect.
func openTestStore(t *testing.T) *db.Store {
	t.Helper()
	config.DB_DIR = t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	q, err := db.ConnectStore(ctx, "test.db", "")
	require.NoError(t, err)

	seedSounds := []struct {
		url     string
		aliases string
	}{
		{"https://example.com/honk.mp4", "honk"},
		{"https://example.com/hejnal.mp4", "hejnał,hejnalmariacki"},
		{"https://example.com/cymbal.mp4", "cymbał,cymbal"},
	}
	for _, s := range seedSounds {
		_, err := q.AddSound(ctx, store.AddSoundParams{
			Gid:     testGID,
			Url:     s.url,
			Aliases: types.Aliases(strings.Split(s.aliases, ",")),
		})
		require.NoError(t, err)
	}

	return q
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
		{"prefix", "hejn", "hejnał"}, // shorter alias wins prefix ties over "hejnalmariacki"
		{"typo", "honkk", "honk"},
		{"multi-alias contains", "cymbal", "cymbal"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sounds, err := findSound(db.Queries, tt.input, testGID)
			require.NoError(t, err)
			require.Len(t, sounds, 1)
			require.Contains(t, sounds[0].Aliases, tt.wantAlias)
		})
	}

	t.Run("no match", func(t *testing.T) {
		_, err := findSound(db.Queries, "asdfghjkl", testGID)
		require.True(t, errors.Is(err, ErrSoundNotFound))
	})

	t.Run("short input below prefix minimum", func(t *testing.T) {
		_, err := findSound(db.Queries, "ho", testGID)
		require.True(t, errors.Is(err, ErrSoundNotFound))
	})
}
