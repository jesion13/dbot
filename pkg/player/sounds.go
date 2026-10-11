package player

import (
	"fmt"
	"log/slog"
)

const (
	// also the cap `event N` clamps to, so an accepted request always fits
	MaxSoundsPerSequence = 2137

	// accepted requests allowed to wait behind the one playing
	maxSoundRequests = 32
)

// One sound file; an empty Filepath means "download first".
type SoundTrack struct {
	Title    string
	URL      string
	Filepath string
}

// An ordered run that must play without another request interleaving: `event 13` is
// one sequence of 13, not 13 separate requests.
type SoundSequence struct {
	Tracks []SoundTrack
}

func (p *Player) EnqueueSound(s SoundTrack) error {
	return p.EnqueueSoundSequence(SoundSequence{Tracks: []SoundTrack{s}})
}

// Appends one atomic request to the sound FIFO; the sequence is copied, so the caller
// may reuse its slice. Music is suspended as soon as the backlog becomes non-empty
// and stays suspended until it drains.
func (p *Player) EnqueueSoundSequence(seq SoundSequence) error {
	if len(seq.Tracks) == 0 {
		return ErrEmptySoundSequence
	}
	if len(seq.Tracks) > MaxSoundsPerSequence {
		return fmt.Errorf("%w: %d sounds", ErrSoundSequenceTooLong, len(seq.Tracks))
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return ErrClosed
	}
	if len(p.soundQ) >= maxSoundRequests {
		p.mu.Unlock()
		return ErrSoundQueueFull
	}
	if !p.soundActive {
		p.soundActive = true
		// before the first sound, so back-to-back requests never let music slip through
		p.gate.Broadcast()
		// no speaking-flag flicker across the handoff
		p.refreshOutputLocked()
	}
	p.soundQ = append(p.soundQ, SoundSequence{Tracks: append([]SoundTrack(nil), seq.Tracks...)})
	p.mu.Unlock()

	select {
	case p.soundSignal <- struct{}{}:
	default:
	}
	return nil
}

func (p *Player) soundLoop() {
	for {
		select {
		case <-p.soundSignal:
		case <-p.baseCtx.Done():
			return
		}

		for {
			p.mu.Lock()
			if p.closed || len(p.soundQ) == 0 {
				p.mu.Unlock()
				break
			}
			seq := p.soundQ[0]
			p.soundQ = p.soundQ[1:]
			p.mu.Unlock()

			p.playSequence(seq)
		}
	}
}

// A sound that fails is logged and skipped so the rest of the sequence still runs, and
// so music is always released afterwards.
func (p *Player) playSequence(seq SoundSequence) {
	defer p.releaseSounds()

	for _, s := range seq.Tracks {
		if p.baseCtx.Err() != nil {
			return
		}
		if err := p.playSoundFn(p.baseCtx, s); err != nil {
			slog.Error("failed to play sound", slog.String("sound", s.Title), slog.Any("error", err))
		}
	}
}

func (p *Player) releaseSounds() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	if len(p.soundQ) > 0 {

		return
	}
	p.soundActive = false
	p.gate.Broadcast()
	// still-eligible music keeps the speaking flag, so resuming is seamless
	p.refreshOutputLocked()
}
