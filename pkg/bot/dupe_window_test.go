package dbot

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"log/slog"
	"math/bits"
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

// noiseImage returns a deterministic pseudo-random image for the seed.
//
// Noise rather than a "realistic" gradient on purpose: the dHash downscales to 9x8 and
// compares horizontally adjacent cells, so only structure that survives that downscale
// reaches the hash. Structured generators tried here (sine fields, blocky luminance
// grids) all collapsed to a handful of independent bits and collided at distance 0,
// because after averaging they left rows that were identical or monotone. Noise keeps
// all 64 bits uncorrelated, so distinct seeds land ~32 bits apart and a re-encoded copy
// of the same image stays within a few bits - exactly the separation dupeThreshold needs.
//
// The corpus that actually matters is the real pictures in the archive; this is only a
// stand-in so the banding test has deterministic inputs.
func noiseImage(t *testing.T, seed int) image.Image {
	t.Helper()
	const w, h = 64, 64
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	// per-seed independent stream, so neighbouring seeds share no structure
	state := uint64(seed)*0x9E3779B97F4A7C15 + 0x243F6A8885A308D3
	next := func() uint64 {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		return state
	}
	for y := range h {
		for x := range w {
			lum := uint8(next())
			img.Set(x, y, color.RGBA{R: lum, G: lum, B: lum, A: 255})
		}
	}
	return img
}

// storeHash is generateHash over an encoded PNG, i.e. exactly what backupArtefact puts
// in the artefacts table for an uploaded picture.
func storeHash(t *testing.T, img image.Image) uint64 {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	h, err := generateHash(PNG, bytes.NewReader(buf.Bytes()))
	require.NoError(t, err)
	return h
}

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

// bruteForceDupe is the reference implementation: every row in the window, real Hamming,
// no prefilter. checkHashAgainstDBSkip must agree with it exactly.
func bruteForceDupe(t *testing.T, tb *testBot, gid string, hash uint64, skipURL string) bool {
	t.Helper()
	all, err := tb.Backup.AllArtefacts(tb.Ctx, gid)
	require.NoError(t, err)
	for _, a := range all {
		if a.OriginUrl == skipURL {
			continue
		}
		if bits.OnesCount64(hash^uint64(a.Hash)) <= dupeThreshold {
			return true
		}
	}
	return false
}

// The banding prefilter drops rows inside SQLite, so it must never change an answer.
// Compared against a full scan rather than against a fixed expectation: whether a
// recompressed copy lands within dupeThreshold depends on the picture, and noise is the
// worst case (measured: 3 bits at q95, 4 at q85). Asserting "always a dupe" would be
// testing JPEG, not the prefilter.
func TestBandingAgreesWithFullScan(t *testing.T) {
	tb := newTestBot(t)

	for seed := range 40 {
		img := noiseImage(t, seed)
		tb.insertImageArtefact(t, fmt.Sprintf("orig-%d.png", seed), storeHash(t, img), time.Now())

		var buf bytes.Buffer
		require.NoError(t, jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}))
		reHash, err := generateHash(JPEG, bytes.NewReader(buf.Bytes()))
		require.NoError(t, err)

		// the recompressed copy is also stored, so the query has something to find
		tb.insertImageArtefact(t, fmt.Sprintf("re-%d.jpg", seed), reHash, time.Now())

		// query with the original hash: must find the recompressed row, not just itself
		q := storeHash(t, img)
		_, unique := tb.checkHashAgainstDBSkip(testGID, q, fmt.Sprintf("orig-%d.png", seed))
		brute := bruteForceDupe(t, tb, testGID, q, fmt.Sprintf("orig-%d.png", seed))
		assert.Equalf(t, !brute, unique, "seed %d: banded and full scan disagree", seed)
	}
}

// Every distinct image must stay distinct: banding must not manufacture false positives.
func TestBandingDoesNotInventMatches(t *testing.T) {
	tb := newTestBot(t)

	for seed := range 60 {
		tb.insertImageArtefact(t, fmt.Sprintf("o-%d.png", seed), storeHash(t, noiseImage(t, seed)), time.Now())
	}

	for seed := range 60 {
		art, unique := tb.checkHashAgainstDB(testGID, storeHash(t, noiseImage(t, seed+1000)))
		assert.Truef(t, unique, "seed %d: an unrelated image reported as %s", seed, art.OriginUrl)
	}
}

// The prefilter is only lossless while there are strictly more bands than the threshold
// allows flipped bits. If someone widens dupeThreshold without widening dupeBands, the
// scan starts silently missing duplicates.
func TestDupeBandingCoversThreshold(t *testing.T) {
	assert.Greater(t, dupeBands, dupeThreshold,
		"pigeonhole needs more bands than the threshold permits flipped bits")

	// prove it rather than trusting the inequality: exhaustively flip every bit count up
	// to the threshold and confirm at least one band survives intact.
	for seed := range 500 {
		h := storeHash(t, noiseImage(t, seed))
		bands := dupeBandValues(h)
		for flip := range dupeThreshold + 1 {
			mutated := h
			for b := range flip {
				mutated ^= 1 << ((seed*7 + b) % 64)
			}
			mb := dupeBandValues(mutated)
			same := false
			for i := range dupeBands {
				if bands[i].Int64 == mb[i].Int64 {
					same = true
				}
			}
			assert.Truef(t, same, "seed %d flip %d: no band survived, a real match would be lost", seed, flip)
		}
	}
}

// dupeBandValues must agree with the generated band0..band3 columns SQLite computes.
// Stored hashes go through int64, so the high-bit cases are the ones that can break:
// SQLite's >> is arithmetic and sign-extends, and Go's is too, so the two must agree.
func TestDupeBandValuesMatchGeneratedColumns(t *testing.T) {
	tb := newTestBot(t)

	for i, h := range []uint64{0, 1, 0xFFFF, 1 << 48, 0x8000000000000000, ^uint64(0)} {
		url := fmt.Sprintf("h%d", i)
		tb.insertImageArtefact(t, url, h, time.Now())

		got, err := tb.Backup.GetArtefact(tb.Ctx, url)
		require.NoError(t, err)
		want := dupeBandValues(h)
		assert.Equalf(t, want[0].Int64, got.Band0.Int64, "hash %016x band0", h)
		assert.Equalf(t, want[1].Int64, got.Band1.Int64, "hash %016x band1", h)
		assert.Equalf(t, want[2].Int64, got.Band2.Int64, "hash %016x band2", h)
		assert.Equalf(t, want[3].Int64, got.Band3.Int64, "hash %016x band3", h)
	}
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
