package jobrunner

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"sync"
	"time"

	"dbot/pkg/store"
)

type Runner struct {
	ctx   context.Context
	store *store.Queries
	jobs  *sync.Map
}

func NewRunner(ctx context.Context, store *store.Queries) Runner {
	return Runner{
		ctx:   ctx,
		store: store,
		jobs:  &sync.Map{},
	}
}

type Job func(meta string) error

// Parked under status "unavailable" instead of consuming retries.
var ErrPermanent = errors.New("permanent job failure")

func (r Runner) RegisterJob(key string, j Job) {
	r.jobs.Store(key, j)
}

func must[T any](v T, a bool) T {
	if !a {
		panic("job does not exist")
	}
	return v
}

func (r Runner) getJob(key string) Job {
	return must(r.jobs.Load(key)).(Job)
}

func jobResult(entry store.Queue, err error) store.UpdateQueueEntryParams {
	updated := store.UpdateQueueEntryParams{
		FailCount: entry.FailCount,
		Status:    "done",
		ID:        entry.ID,
	}
	switch {
	case err == nil:
	case errors.Is(err, ErrPermanent):
		slog.Info("job failed permanently", slog.Any("error", err))

		updated.Status = "unavailable"
		updated.LastMsg = sql.NullString{String: err.Error(), Valid: true}
	default:
		slog.Error("failed running job", slog.Any("error", err))
		updated.FailCount = entry.FailCount + 1
		updated.Status = "failing"
		updated.LastMsg = sql.NullString{String: err.Error(), Valid: true}
	}
	return updated
}

//	type Queue struct {
//		ID        int64
//		Meta      string
//		FailCount int64
//		Status    string
//		JobType   string
//		LastMsg   sql.NullString
//	}
func (r Runner) Loop() {
	for {
		var job Job
		var updated store.UpdateQueueEntryParams
		entry, err := r.store.NextInQueue(r.ctx)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				time.Sleep(time.Second * 5)
				goto Select
			}
			slog.Error("failed to get item from queue", slog.Any("error", err))
			time.Sleep(5 * time.Second)
			goto Select
		}

		job = r.getJob(entry.JobType)
		slog.Debug("running job", slog.String("key", entry.JobType), slog.String("meta", entry.Meta))
		updated = jobResult(entry, job(entry.Meta))
		err = r.store.UpdateQueueEntry(r.ctx, updated)
		if err != nil {
			slog.Error("failed updating queue entry", slog.Any("error", err))
		}

	Select:
		select {
		case <-r.ctx.Done():
			return
		default:
		}
	}
}

// exposes jobResult so other packages can assert the retry policy against the SQL
// filters in db/queries/queue.sql
func JobResultForTest(entry store.Queue, err error) store.UpdateQueueEntryParams {
	return jobResult(entry, err)
}
