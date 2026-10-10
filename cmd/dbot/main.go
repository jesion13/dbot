package main

import (
	"context"
	"os"
	"os/signal"
	"time"

	"dbot/pkg/api"
	dbot "dbot/pkg/bot"
	"dbot/pkg/config"
	"dbot/pkg/db"
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
	client.Close(ctx)
	time.Sleep(time.Second)
}
