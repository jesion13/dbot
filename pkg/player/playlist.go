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

// QueueSnapshot is a point-in-time copy of the player's lineup.
//
// Tracks is the complete lineup for this player's lifetime, in original order,
// including everything that already played. Position N of the lineup is 1-based
// index N in this slice, and that mapping never changes for an entry: tracks are
// only ever appended. That is what makes a position worth showing a user, and it is
// what Next, Previous and JumpTo all index.
//
// There is deliberately no per-entry state. Playback position already says where we
// are; a second copy of "did this one play" could only drift from the cursor.
//
// Everything here is a copy, so callers cannot reach into the live lineup.
type QueueSnapshot struct {
	Tracks []Track
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

// Entry returns the track at a 1-based absolute position, nil when out of range.
func (s QueueSnapshot) Entry(position int) *Track {
	if position < 1 || position > len(s.Tracks) {
		return nil
	}
	return &s.Tracks[position-1]
}

// Current returns the playing track, nil when idle.
func (s QueueSnapshot) Current() *Track {
	return s.Entry(s.CurrentIndex)
}

// Len is the number of entries in the lineup.
func (s QueueSnapshot) Len() int { return len(s.Tracks) }
