package dbot

import (
	"fmt"
	"time"

	"dbot/pkg/store"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
	"log/slog"
)

func (d *DBot) interfaceLoop() {
	tic := time.NewTicker(time.Second * 3)
	go func() {
		var lastContent string
		var lastID snowflake.ID
		for range tic.C {
			if d.MusicPlayer.VC == nil {
				continue
			}
			c, err := d.Store.GetChannel(d.Ctx, store.GetChannelParams{
				Gid:  d.MusicPlayer.VC.GuildID().String(),
				Type: "music",
			})
			if err != nil {
				continue
			}
			current := d.MusicPlayer.Current()
			if current == nil {
				continue
			}

			content := fmt.Sprintf("Playing [%s](%s)", current.Title, current.Link)
			if content == lastContent {
				continue
			}
			chID := snowflake.MustParse(c.Chid)
			err = d.Client.Rest.DeleteMessage(chID, lastID)
			if err != nil {
				slog.Error("failed deleting last message", slog.Any("error", err), slog.String("err_type", fmt.Sprintf("%T", err)))
			}

			lastContent = content
			msg, err := d.Client.Rest.CreateMessage(chID, discord.MessageCreate{Content: content})
			if err != nil {
				slog.Error("failed sending message", slog.Any("error", err), slog.String("err_type", fmt.Sprintf("%T", err)))
				continue
			}
			lastID = msg.ID
		}
	}()
}
