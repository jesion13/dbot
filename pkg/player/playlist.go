package player

// Track is one queued item. URL is where it came from, Filepath the local file to
// play; an empty Filepath means "stream from URL". Neither field can be derived
// from the other, so they are kept apart.
type Track struct {
	ID       uint64
	Title    string
	URL      string
	Filepath string
}

// QueueSnapshot is a point-in-time copy of the player state. Everything it holds
// is a copy: callers cannot reach into the live queue through it.
type QueueSnapshot struct {
	// Current is the playing track, nil when idle.
	Current *Track
	// Upcoming is the FIFO of tracks still to play.
	Upcoming []Track
	// History is the most recently played tracks, newest last.
	History []Track
	// Paused is the user's own pause, independent of sound suspension.
	Paused bool
	// SoundWait is the number of accepted sound requests still to play.
	SoundWait int
	// Closed reports whether the player has been shut down.
	Closed bool
}

// orderedLocked lists the current track as position 1 followed by the upcoming
// queue, which is exactly what JumpTo indexes. Idle: upcoming only, so position 1
// is the next track to play.
func (p *Player) orderedLocked() []*Track {
	if p.current == nil {
		return p.queue
	}
	out := make([]*Track, 0, len(p.queue)+1)
	out = append(out, p.current)
	return append(out, p.queue...)
}

// pushHistoryLocked records a track that was playing, newest last, bounded so a
// long session cannot grow it without limit.
func (p *Player) pushHistoryLocked(t Track) {
	p.history = append(p.history, &t)
	if len(p.history) > historyMax {
		p.history = append(p.history[:0], p.history[len(p.history)-historyMax:]...)
	}
}
