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

// QueueState is what happened to one lineup entry. It is tracked per entry rather
// than inferred from the position, because a track before the cursor may have been
// jumped over rather than played.
type QueueState int

const (
	// StateUpcoming means it has not been reached yet.
	StateUpcoming QueueState = iota
	// StatePlaying is the entry the cursor is on.
	StatePlaying
	// StatePlayed means it ran to the end on its own.
	StatePlayed
	// StateSkipped means playback moved past it without finishing.
	StateSkipped
)

// QueueEntry is one absolute position in the player's lineup.
type QueueEntry struct {
	Track
	State QueueState
}

// QueueSnapshot is a point-in-time copy of the player's lineup.
//
// Entries is the complete lineup for this player's lifetime, in original order,
// including everything already played. Position N of the lineup is 1-based index N
// in this slice, and that mapping never changes for an entry: tracks are only ever
// appended. This is what makes a reported position stable and worth showing to a
// user, and what JumpTo indexes.
//
// Everything here is a copy, so callers cannot reach into the live lineup.
type QueueSnapshot struct {
	Entries []QueueEntry
	// CurrentIndex is the absolute 1-based position of the playing entry, or 0 when
	// the player is idle.
	CurrentIndex int
	// Paused is the user's own pause, independent of sound suspension.
	Paused bool
	// SoundWait is the number of accepted sound requests still to play.
	SoundWait int
	// Closed reports whether the player has been shut down.
	Closed bool
}

// Entry returns the entry at a 1-based absolute position, nil when out of range.
func (s QueueSnapshot) Entry(position int) *QueueEntry {
	if position < 1 || position > len(s.Entries) {
		return nil
	}
	return &s.Entries[position-1]
}

// Current returns the playing entry, nil when idle.
func (s QueueSnapshot) Current() *QueueEntry {
	return s.Entry(s.CurrentIndex)
}

// Len is the number of entries in the lineup.
func (s QueueSnapshot) Len() int { return len(s.Entries) }

// lineupEntry is the player's private view of one position.
type lineupEntry struct {
	track Track
	state QueueState
}

// pushHistoryLocked records a track that finished, newest last, bounded so a long
// session cannot grow it without limit. History is diagnostics only: it is never
// used to decide positions, and dropping old entries from it never evicts anything
// from the lineup.
func (p *Player) pushHistoryLocked(t Track) {
	p.history = append(p.history, t)
	if len(p.history) > historyMax {
		p.history = append(p.history[:0], p.history[len(p.history)-historyMax:]...)
	}
}
