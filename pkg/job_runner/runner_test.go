package jobrunner

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"dbot/pkg/store"

	"github.com/stretchr/testify/assert"
)

func TestJobResult(t *testing.T) {
	entry := store.Queue{ID: 7, FailCount: 2}

	t.Run("success", func(t *testing.T) {
		got := jobResult(entry, nil)
		assert.Equal(t, "done", got.Status)
		assert.Equal(t, int64(2), got.FailCount, "a success must not consume retries")
		assert.False(t, got.LastMsg.Valid)
	})

	t.Run("permanent failure is parked without burning retries", func(t *testing.T) {
		got := jobResult(entry, fmt.Errorf("%w: video unavailable", ErrPermanent))
		assert.Equal(t, "unavailable", got.Status)
		assert.Equal(t, int64(2), got.FailCount)
		assert.True(t, got.LastMsg.Valid)
	})

	t.Run("transient failure counts a retry", func(t *testing.T) {
		got := jobResult(entry, errors.New("boom"))
		assert.Equal(t, "failing", got.Status)
		assert.Equal(t, int64(3), got.FailCount)
		assert.Equal(t, sql.NullString{String: "boom", Valid: true}, got.LastMsg)
	})

	t.Run("statuses stay in sync with the queue query", func(t *testing.T) {
		// pkg/bot reads the newest job row and treats status 'unavailable' as terminal;
		// NextInQueue skips both 'done' and 'unavailable'.
		assert.Equal(t, "unavailable", jobResult(entry, ErrPermanent).Status)
		assert.Equal(t, "failing", jobResult(entry, errors.New("x")).Status)
		assert.Equal(t, "done", jobResult(entry, nil).Status)
	})

	t.Run("the third failure reaches the attempt budget", func(t *testing.T) {
		e := store.Queue{ID: 1, FailCount: 2}
		got := jobResult(e, errors.New("boom"))
		assert.Equal(t, int64(3), got.FailCount)
		assert.Equal(t, "failing", got.Status)
	})
}
