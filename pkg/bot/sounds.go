package dbot

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"math/big"
	"strconv"
	"strings"
	"sync"

	"dbot/pkg/store"

	fuzzy "github.com/paul-mannino/go-fuzzywuzzy"
)

var ErrSoundNotFound = errors.New("sound not found")

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

func (srq *soundsRandQ) rand(db *store.Queries, gid string) store.Sound {
	srq.refresh(db, gid)
	srq.Lock()
	defer srq.Unlock()
	sounds := srq.s[gid]
	randInt, _ := rand.Int(rand.Reader, big.NewInt(int64(len(sounds))))

	slog.Debug("soundsRandQ.rand", slog.Any("srq.s[gid]", len(srq.s[gid])), slog.Any("randInt", randInt))
	v := sounds[randInt.Int64()]

	sounds[randInt.Int64()] = sounds[len(sounds)-1]
	sounds = sounds[:len(sounds)-1]
	srq.s[gid] = sounds
	return v
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
// Longer aliases win ties within prefix/contains so "cymbal" beats a shorter false prefix.
func matchScore(alias, name string) int {
	if alias == name {
		return 1000
	}
	if len(name) >= minPrefixLen && strings.HasPrefix(alias, name) {
		return 200 + len(alias)
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
			return append(ss, srq.rand(db, gid)), nil
		}

		num := name[5:]
		numInt, err := strconv.Atoi(num)
		if err != nil {
			return append(ss, srq.rand(db, gid)), nil
		}

		for range min(numInt, 2137) {
			ss = append(ss, srq.rand(db, gid))
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
