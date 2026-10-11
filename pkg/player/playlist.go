package player

// One queued item. An empty Filepath means "stream from URL"; neither field can be
// derived from the other, so they are kept apart.
type Track struct {
	ID       uint64
	Title    string
	URL      string
	Filepath string
}

// A point-in-time copy of the lineup, in original order and including everything that
// already played. Position N is 1-based index N in Tracks, and never changes for an
// entry - which is what makes a position worth showing a user, and what Next,
// Previous and JumpTo all index.
//
// There is deliberately no per-entry state: playback position already says where we
// are, and a second copy of "did this one play" could only drift from the cursor.
type QueueSnapshot struct {
	Tracks []Track
	// 1-based position of the playing entry, or 0 when idle
	CurrentIndex int
	// the user's own pause, independent of sound suspension
	Paused bool
	// accepted sound requests still to play
	SoundWait int
	Closed    bool
}

// The track at a 1-based absolute position, nil when out of range.
func (s QueueSnapshot) Entry(position int) *Track {
	if position < 1 || position > len(s.Tracks) {
		return nil
	}
	return &s.Tracks[position-1]
}

// The playing track, nil when idle.
func (s QueueSnapshot) Current() *Track {
	return s.Entry(s.CurrentIndex)
}

func (s QueueSnapshot) Len() int { return len(s.Tracks) }
