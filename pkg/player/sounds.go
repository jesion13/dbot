package player

import (
	"fmt"
	"log/slog"
)

const (
	// MaxSoundsPerSequence bounds one accepted sound request. It is also the cap
	// the `event N` command clamps N to, so an accepted request always fits.
	MaxSoundsPerSequence = 2137
	// maxSoundRequests bounds how many accepted requests may wait behind the one
	// playing.
	maxSoundRequests = 32
)

// SoundTrack is one sound file. URL is the source, Filepath the local file; an
// empty Filepath means "download first".
type SoundTrack struct {
	Title    string
	URL      string
	Filepath string
}

// SoundSequence is an ordered run of sounds that must play without another
// request interleaving between them. `event 13` is one sequence of 13, not 13
// separate requests.
type SoundSequence struct {
	Tracks []SoundTrack
}

// EnqueueSound appends a single sound as a one-item sequence.
func (p *Player) EnqueueSound(s SoundTrack) error {
	return p.EnqueueSoundSequence(SoundSequence{Tracks: []SoundTrack{s}})
}

// EnqueueSoundSequence appends one atomic request to the sound FIFO. The sequence
// is copied, so the caller may reuse its slice. Music is suspended as soon as the
// backlog becomes non-empty and stays suspended until it drains again.
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
		// suspend before the first sound starts, so back-to-back requests never
		// let music slip through between them
		p.gate.Broadcast()
		// the output stays active across the handoff: music hands over to the sound
		// without dropping the speaking flag
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

// soundLoop is the only sound producer: one request at a time, one sequence at a
// time, in FIFO order.
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

// playSequence plays every sound of one request in order. A sound that fails is
// logged and skipped so the rest of the sequence still runs, and so music is
// always released afterwards.
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

// releaseSounds lifts the music suspension once the whole backlog is drained, and
// keeps it in place while another request is still waiting.
func (p *Player) releaseSounds() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	if len(p.soundQ) > 0 {
		// the next sequence takes over directly, music stays suspended
		return
	}
	p.soundActive = false
	p.gate.Broadcast()
	// music that is still eligible keeps the output (and the speaking flag) as it
	// was, so resuming is seamless
	p.refreshOutputLocked()
}
