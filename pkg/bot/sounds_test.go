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

func TestFindSoundEvent(t *testing.T) {
	// `event N` is one request holding N sounds, so it must return exactly N.
	// srq pools the library per guild and hands sounds out without replacement, so
	// every case reloads it first.
	db := openTestStore(t)
	srq.s = map[string][]store.Sound{}
	reset := func() { srq.dropAndLoad(db.Queries, testGID) }

	t.Run("count", func(t *testing.T) {
		reset()
		sounds, err := findSound(db.Queries, "event 3", testGID)
		require.NoError(t, err)
		require.Len(t, sounds, 3)
	})

	t.Run("sound N works the same", func(t *testing.T) {
		reset()
		sounds, err := findSound(db.Queries, "sound2", testGID)
		require.NoError(t, err)
		require.Len(t, sounds, 2)
	})

	t.Run("non numeric falls back to one random sound", func(t *testing.T) {
		reset()
		sounds, err := findSound(db.Queries, "eventalot", testGID)
		require.NoError(t, err)
		require.Len(t, sounds, 1)
	})

	t.Run("asking for more than the library holds repeats, never panics", func(t *testing.T) {
		reset()
		sounds, err := findSound(db.Queries, "event 500", testGID)
		require.NoError(t, err)
		require.Len(t, sounds, 500, "the library is reloaded as it empties")
	})

	t.Run("a guild without sounds is an error, not a panic", func(t *testing.T) {
		_, err := findSound(db.Queries, "event 3", "empty-guild")
		require.ErrorIs(t, err, ErrSoundNotFound)
	})

	t.Run("a zero count is rejected", func(t *testing.T) {
		reset()
		_, err := findSound(db.Queries, "event0", testGID)
		require.ErrorIs(t, err, ErrInvalidSoundCount)
	})
}
