package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"time"

	"dbot/pkg/api"
	dbot "dbot/pkg/bot"
	"dbot/pkg/config"
	"dbot/pkg/db"
	"dbot/pkg/logger"
	"dbot/pkg/store"
	"dbot/pkg/ytdlp"

	"github.com/disgoorg/disgo"
	disgobot "github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/godave/golibdave"
)

func main() {
	config.Load()
	slog.SetDefault(logger.New(os.Stdout, logger.ParseLevel(config.LOG_LEVEL)))
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	ytdlp.StartUpdater(ctx)

	db, err := db.ConnectStore(ctx, "./test.db", "")
	if err != nil {
		panic(err)
	}

	bot(ctx, db)
}

func bot(ctx context.Context, db *store.Queries) {
	client, err := disgo.New(config.TOKEN,
		disgobot.WithLogger(logger.New(os.Stdout, slog.LevelWarn)),
		disgobot.WithEventManagerConfigOpts(
			disgobot.WithAsyncEventsEnabled(),
		),
		disgobot.WithGatewayConfigOpts(
			gateway.WithIntents(gateway.IntentsNonPrivileged, gateway.IntentMessageContent),
		),
		disgobot.WithCacheConfigOpts(
			cache.WithCaches(cache.FlagVoiceStates, cache.FlagMembers),
		),
		disgobot.WithVoiceManagerConfigOpts(
			voice.WithDaveSessionCreateFunc(golibdave.NewSession),
		),
	)
	if err != nil {
		panic(err)
	}

	d := dbot.Start(ctx, client, db)
	api.StartServer(d)

	<-ctx.Done()
	closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client.Close(closeCtx)
	time.Sleep(time.Second)
}
