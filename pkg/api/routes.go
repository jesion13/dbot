package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	dbot "dbot/pkg/bot"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
	"github.com/fr-str/log"
)

func StartServer(d *dbot.DBot) {
	http.HandleFunc("/api/dst/links", func(w http.ResponseWriter, r *http.Request) {
		links := make(map[string]string)
		err := json.NewDecoder(r.Body).Decode(&links)
		if err != nil {
			log.Error(err.Error())
			return
		}
		if len(links) == 0 {
			log.Error("no links")
			return
		}
		log.Trace("links", log.JSON(links))

		var msg strings.Builder
		msg.WriteString("New	links:\n")
		for k, v := range links {
			fmt.Fprintf(&msg, "[URL](%s) Points: %s\n", k, v)
		}

		_, err = d.Client.Rest.CreateMessage(snowflake.MustParse("983810486627876924"), discord.MessageCreate{Content: msg.String()})
		if err != nil {
			log.Error(err.Error())
		}

		w.WriteHeader(http.StatusOK)
	})
	http.HandleFunc("/api/init-scrape", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
		go func() {
			var gID snowflake.ID

			channels, err := d.Client.Rest.GetGuildChannels(gID)
			if err != nil {
				panic(err)
			}
			for _, c := range channels {
				fmt.Println(c.Name())
			}
			for _, ch := range channels {
				if ch.Type() != discord.ChannelTypeGuildText {
					continue
				}
				log.Trace("[dupa]", log.Any("ch.Name", ch.Name()))
				var lastID snowflake.ID
				for {
					msgs, err := d.Client.Rest.GetMessages(ch.ID(), 0, lastID, 0, 100)
					if err != nil {
						fmt.Println(err)
						return
					}
					if len(msgs) > 0 {
						lastID = msgs[len(msgs)-1].ID
					}
					for _, msg := range msgs {
						msg.GuildID = &gID
						if len(msg.Attachments) > 0 {
							dbot.BackupAttachment(d, msg)
						}
					}
					if len(msgs) < 100 {
						break
					}
				}
			}
			log.Info("DONE")
		}()
	})
	go func() {
		log.Info("starting server: http://localhost:58008")
		err := http.ListenAndServe(":58008", nil)
		if err != nil {
			log.Error(err.Error())
		}
	}()
}
