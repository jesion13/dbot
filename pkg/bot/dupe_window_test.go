package dbot

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dbot/pkg/backup"
	"dbot/pkg/config"
	dbotlog "dbot/pkg/logger"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// patternedImage encodes to bytes so a real perceptual hash can be stored for it. It
// has to vary: a flat colour has no gradient and hashes to zero.
func patternedImage(t *testing.T, seed int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	for x := range 32 {
		for y := range 32 {
			img.Set(x, y, color.RGBA{
				R: uint8((x*7 + seed) % 256),
				G: uint8((y*5 + seed) % 256),
				B: uint8((x + y + seed) % 256),
				A: 255,
			})
		}
	}
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

// insertImageArtefact stores a real image artefact with a chosen age.
func (tb *testBot) insertImageArtefact(t *testing.T, originURL string, hash uint64, createdAt time.Time) {
	t.Helper()
	err := tb.Backup.InsertArtefact(tb.Ctx, backup.InsertArtefactParams{
		OriginUrl: originURL,
		Path:      "img/" + originURL + ".png",
		MediaType: PNG,
		Hash:      int64(hash),
		CreatedAt: createdAt,
		Gid:       testGID,
		Chid:      "c",
		Msgid:     "m",
	})
	require.NoError(t, err)
}

// The dupe scan must not walk the whole archive: only a recent window is compared.
func TestCheckHashOnlyLooksAtTheRecentWindow(t *testing.T) {
	tb := newTestBot(t)

	// one real image, stored just outside the window
	img := patternedImage(t, 3)
	full := filepath.Join(config.BACKUP_DIR, "img", "old.png")
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(t, os.WriteFile(full, img, 0o644))

	hash, err := generateHash(PNG, bytes.NewReader(img))
	require.NoError(t, err)
	require.NotZero(t, hash)

	tb.insertImageArtefact(t, "old", hash, time.Now().Add(-dupeScanWindow-time.Hour))

	art, unique := tb.checkHashAgainstDB(testGID, hash)
	assert.True(t, unique, "an image older than the window is not a duplicate")
	assert.Empty(t, art.OriginUrl)

	// the same image inside the window is found
	tb.insertImageArtefact(t, "recent", hash, time.Now().Add(-dupeScanWindow+time.Hour))
	art, unique = tb.checkHashAgainstDB(testGID, hash)
	assert.False(t, unique, "a recent image with the same hash is a duplicate")
	assert.Equal(t, "recent", art.OriginUrl)
}

// Nothing in the window at all must not be mistaken for a dupe.
func TestCheckHashWithNoRecentImages(t *testing.T) {
	tb := newTestBot(t)

	_, unique := tb.checkHashAgainstDB(testGID, 12345)
	assert.True(t, unique)
}

func TestDupeScanWindowIsSixMonths(t *testing.T) {
	// the window is a plain duration; pin it so it cannot drift silently
	assert.Equal(t, 180*24*time.Hour, dupeScanWindow)
}

// --- planQueue must not query per entry --------------------------------------

// Counting the queries proves the batch is one round trip rather than one per entry.
// Every statement in this repo is logged at dbotlog.LevelSQL, so a handler that counts them is
// enough - no second connection and no wrapper needed.
func TestPlanQueueBatchesItsLookups(t *testing.T) {
	for _, n := range []int{1, 5, 20} {
		t.Run(plural(n), func(t *testing.T) {
			tb := newTestBot(t)
			ids := make([]string, 0, n)
			for i := range n {
				ids = append(ids, "vid"+itoa(int64(i)))
			}
			pl, err := tb.saveSnapshot(tb.Ctx, testGID, testPlaylistMeta(t, "PL", "p", ids...), false)
			require.NoError(t, err)
			entries := tb.entries(t, pl.ID)
			require.Len(t, entries, n)

			count := countSQLQueries(t, func() {
				_, _, err = tb.planQueue(tb.Ctx, testGID, entries)
			})
			require.NoError(t, err)

			// Two batch reads plus one enqueue per uncached entry: measured 3/7/22 for
			// 1/5/20 entries. Looking each entry up separately measured 6/22/82, so this
			// bound fails if the batching is undone.
			assert.LessOrEqual(t, count, n+2,
				"%d entries must not cost a query per entry", n)
		})
	}
}

// countSQLQueries runs fn and reports how many statements it issued.
func countSQLQueries(t *testing.T, fn func()) int {
	t.Helper()
	c := &sqlCounter{}
	prev := slog.Default()
	slog.SetDefault(slog.New(c))
	t.Cleanup(func() { slog.SetDefault(prev) })
	fn()
	return c.n
}

type sqlCounter struct{ n int }

func (c *sqlCounter) Enabled(context.Context, slog.Level) bool { return true }

func (c *sqlCounter) Handle(_ context.Context, r slog.Record) error {
	if r.Level == dbotlog.LevelSQL {
		c.n++
	}
	return nil
}

func (c *sqlCounter) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *sqlCounter) WithGroup(string) slog.Handler      { return c }

func plural(n int) string {
	if n == 1 {
		return "one entry"
	}
	return "many entries"
}
