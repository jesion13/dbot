package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	dbot "dbot/pkg/bot"

	"github.com/bwmarrin/discordgo"
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

		_, err = d.ChannelMessageSend("983810486627876924", msg.String())
		if err != nil {
			log.Error(err.Error())
		}

		w.WriteHeader(http.StatusOK)
	})
	http.HandleFunc("/api/init-scrape", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
		return
		go func() {
			gID := ""

			channels, err := d.GuildChannels(gID)
			if err != nil {
				panic(err)
			}
			for _, c := range channels {
				fmt.Println(c.Name)
			}
			for _, ch := range channels {
				if ch.Type != discordgo.ChannelTypeGuildText {
					continue
				}
				log.Trace("[dupa]", log.Any("ch.Name", ch.Name))
				lastID := ""
				for {
					msgs, err := d.ChannelMessages(ch.ID, 100, lastID, "", "")
					if err != nil {
						fmt.Println(err)
						return
					}
					if len(msgs) > 0 {
						lastID = msgs[len(msgs)-1].ID
					}
					for _, msg := range msgs {
						msg.GuildID = gID
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
