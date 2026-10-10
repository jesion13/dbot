package dbot

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strconv"
	"strings"
	"sync"

	"dbot/pkg/player"
	"dbot/pkg/store"

	fuzzy "github.com/paul-mannino/go-fuzzywuzzy"
)

var (
	ErrSoundNotFound     = errors.New("sound not found")
	ErrNoSoundsLeft      = errors.New("no sounds left in the library")
	ErrInvalidSoundCount = errors.New("sound count must be positive")
)

type soundsRandQ struct {
	sync.Mutex
	s map[string][]store.Sound
}

func (srq *soundsRandQ) refresh(db *store.Queries, gid string) {
	srq.Lock()
	defer srq.Unlock()
	if len(srq.s[gid]) != 0 {
		return
	}
	sounds, err := db.SelectSounds(context.Background(), gid)
	if err != nil {
		slog.Error("db select failed", slog.Any("error", err))
	}
	srq.s[gid] = sounds
}

func (srq *soundsRandQ) dropAndLoad(db *store.Queries, gid string) {
	srq.Lock()
	srq.s[gid] = nil
	srq.Unlock()
	srq.refresh(db, gid)
}

func (srq *soundsRandQ) rand(db *store.Queries, gid string) (store.Sound, error) {
	srq.refresh(db, gid)
	srq.Lock()
	defer srq.Unlock()
	sounds := srq.s[gid]
	if len(sounds) == 0 {
		return store.Sound{}, ErrNoSoundsLeft
	}
	randInt, err := rand.Int(rand.Reader, big.NewInt(int64(len(sounds))))
	if err != nil {
		return store.Sound{}, fmt.Errorf("pick random sound: %w", err)
	}

	i := randInt.Int64()
	slog.Debug("soundsRandQ.rand", slog.Int("sounds", len(sounds)), slog.Int64("randInt", i))
	v := sounds[i]

	sounds[i] = sounds[len(sounds)-1]
	sounds = sounds[:len(sounds)-1]
	srq.s[gid] = sounds
	return v, nil
}

func (srq *soundsRandQ) all(db *store.Queries, gid string) []store.Sound {
	srq.refresh(db, gid)
	srq.Lock()
	defer srq.Unlock()
	return append([]store.Sound(nil), srq.s[gid]...)
}

var srq = soundsRandQ{
	s: map[string][]store.Sound{},
}

const (
	minPrefixLen   = 3
	minContainsLen = 4
	fuzzyThreshold = 75
)

// matchScore ranks alias against name: exact > prefix > contains > fuzzy typo tolerance.
// Prefix ties favor the shorter alias (closest completion of what was typed).
// Contains ties favor the longer alias (more exact substring coverage).
func matchScore(alias, name string) int {
	if alias == name {
		return 1000
	}
	if len(name) >= minPrefixLen && strings.HasPrefix(alias, name) {
		return 500 - len(alias)
	}
	if len(name) >= minContainsLen && strings.Contains(alias, name) {
		return 150 + len(alias)
	}
	return fuzzy.Ratio(alias, name)
}

func findSound(db *store.Queries, name string, gid string) ([]store.Sound, error) {
	var ss []store.Sound
	name = strings.ToLower(strings.ReplaceAll(name, " ", ""))
	if strings.HasPrefix(name, "sound") || strings.HasPrefix(name, "event") {
		if len(name) < 5 {
			s, err := srq.rand(db, gid)
			if err != nil {
				return nil, err
			}
			return append(ss, s), nil
		}

		num := name[5:]
		numInt, err := strconv.Atoi(num)
		if err != nil {
			s, rerr := srq.rand(db, gid)
			if rerr != nil {
				return nil, rerr
			}
			return append(ss, s), nil
		}
		if numInt < 1 {
			return nil, ErrInvalidSoundCount
		}

		// one request holding N sounds; capped so the player always accepts it
		for range min(numInt, player.MaxSoundsPerSequence) {
			s, err := srq.rand(db, gid)
			if err != nil {
				// library exhausted mid-sequence: play what was picked, do not panic
				slog.Warn("sound library exhausted", slog.Int("picked", len(ss)), slog.Any("error", err))
				break
			}
			ss = append(ss, s)
		}

		if len(ss) == 0 {
			return nil, ErrSoundNotFound
		}
		return ss, nil
	}

	sounds := srq.all(db, gid)
	var fullSound store.Sound
	var fullScore int
	for _, sound := range sounds {
		for _, alias := range sound.Aliases {
			score := matchScore(alias, name)
			if score > fullScore {
				slog.Debug("sound match", slog.Int("score", score), slog.String("alias", alias))
				fullScore = score
				fullSound = sound
			}
		}
	}

	if fullScore >= fuzzyThreshold {
		return append(ss, fullSound), nil
	}

	return ss, ErrSoundNotFound
}
