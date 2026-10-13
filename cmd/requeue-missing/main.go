// Command requeue-missing queues a download for every cached video whose file is no
// longer on disk.
//
// The bot only re-downloads a missing video when somebody plays it again, so a file
// lost to a bad rsync or a manual tidy sits unnoticed in the database until that exact
// song is wanted. This finds those rows and queues them.
//
// It writes one queue row per missing video; it deletes nothing, moves nothing and
// never touches a file. The job runner is a single serial loop, so expect the queue to
// drain slowly. A url that youtube no longer serves is parked as "unavailable" by the
// existing failure handling rather than retried forever.
//
// Run with -n first to see what would be queued.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"

	botpkg "dbot/pkg/bot"
	"dbot/pkg/config"
	"dbot/pkg/db"
)

func main() {
	dryRun := flag.Bool("n", false, "list what would be queued without writing anything")
	flag.Parse()

	config.Load()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, nil)))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	storeDB, err := db.ConnectStore(ctx, "test.db", "")
	if err != nil {
		panic(err)
	}
	backupDB, err := db.ConnectBackup(ctx, "backup.db", "")
	if err != nil {
		panic(err)
	}

	res, err := botpkg.RequeueMissingArtefacts(ctx, storeDB.Queries, backupDB, *dryRun)
	if err != nil {
		panic(err)
	}

	slog.Info("requeue done",
		slog.Int("scanned", res.Scanned),
		slog.Int("present", res.Present),
		slog.Int("queued", res.Enqueued),
		slog.Int("already_pending", res.Pending),
		slog.Int("unavailable", res.Unavailable),
		slog.Bool("dry_run", *dryRun))
}
