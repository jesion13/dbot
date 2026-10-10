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
		err = job(entry.Meta)
		if err != nil {
			slog.Error("failed running job", slog.Any("error", err))
			updated = store.UpdateQueueEntryParams{
				FailCount: entry.FailCount + 1,
				LastMsg: sql.NullString{
					String: err.Error(),
					Valid:  true,
				},
				Status: "failing",
				ID:     entry.ID,
			}
		} else {
			updated = store.UpdateQueueEntryParams{
				FailCount: entry.FailCount,
				LastMsg:   sql.NullString{String: "", Valid: false},
				Status:    "done",
				ID:        entry.ID,
			}
		}
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
