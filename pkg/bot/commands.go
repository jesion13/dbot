package dbot

import (
	"context"
	"log/slog"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
)

// descryptions are requeired by Discord
var cmds = []discord.ApplicationCommandCreate{
	discord.SlashCommandCreate{
		Name:        "play",
		Description: "play audio from source",
		Options: []discord.ApplicationCommandOption{
			discord.ApplicationCommandOptionString{
				Name:        "link",
				Description: "dupa",
			},
			discord.ApplicationCommandOptionAttachment{
				Name:        "file",
				Description: "file",
			},
		},
	},
	discord.SlashCommandCreate{
		Name:        "pause-play",
		Description: "play pause",
	},
	discord.SlashCommandCreate{
		Name:        "wypierdalaj",
		Description: "bot wpierdala",
	},
	discord.SlashCommandCreate{
		Name:        "to-mp4",
		Description: "Attempts to transcode video to format compatible with discord.",
		Options: []discord.ApplicationCommandOption{
			discord.ApplicationCommandOptionString{
				Name:        "link",
				Description: "link to audio",
			},
			discord.ApplicationCommandOptionAttachment{
				Name:        "file",
				Description: "file",
			},
			discord.ApplicationCommandOptionBool{
				Name:        "mute",
				Description: "Remove audio from the video",
			},
			discord.ApplicationCommandOptionString{
				Name:        "format",
				Description: "Output format",
				Choices: []discord.ApplicationCommandOptionChoiceString{
					{
						Name:  "MP4",
						Value: "mp4",
					},
					{
						Name:  "GIF",
						Value: "gif",
					},
				},
			},
			discord.ApplicationCommandOptionString{
				Name:        "start",
				Description: "Clip start time (e.g., 1:30 or 90)",
			},
			discord.ApplicationCommandOptionString{
				Name:        "end",
				Description: "Clip end time (e.g., 2:00 or 120)",
			},
		},
	},
	discord.SlashCommandCreate{
		Name:        "sound",
		Description: "add new sound",
		Options: []discord.ApplicationCommandOption{
			discord.ApplicationCommandOptionString{
				Name:        "aliases",
				Description: "triggers for sound",
				Required:    true,
			},
			discord.ApplicationCommandOptionString{
				Name:        "link",
				Description: "link to audio",
			},
			discord.ApplicationCommandOptionAttachment{
				Name:        "file",
				Description: "file",
			},
		},
	},
	discord.SlashCommandCreate{
		Name:        "set-bot-channel",
		Description: "sets channels for bot to work in",
		Options: []discord.ApplicationCommandOption{
			discord.ApplicationCommandOptionString{
				Name:        "type",
				Description: "type",
				Required:    true,
			},
			discord.ApplicationCommandOptionChannel{
				Name:        "channel",
				Description: "channel",
				Required:    true,
			},
		},
	},
	discord.SlashCommandCreate{
		Name:        "save-playlist",
		Description: "saves playlist and downloads all avaiable videos",
		Options: []discord.ApplicationCommandOption{
			discord.ApplicationCommandOptionString{
				Name:        "name",
				Description: "name",
				Required:    true,
			},
			discord.ApplicationCommandOptionString{
				Name:        "yt-link",
				Description: "link",
				Required:    true,
			},
		},
	},
	discord.SlashCommandCreate{
		Name:        "load-playlist",
		Description: "loads saved playlist",
		Options: []discord.ApplicationCommandOption{
			discord.ApplicationCommandOptionString{
				Name:         "name",
				Description:  "name",
				Autocomplete: true,
				Required:     true,
			},
		},
	},
}

func (d *DBot) ClearCmds() {
	c, err := d.Client.Rest.GetGuildCommands(d.Client.ApplicationID, d.GuildID, false)
	if err != nil {
		slog.Error(err.Error())
	}

	for _, v := range c {
		err := d.Client.Rest.DeleteGuildCommand(d.Client.ApplicationID, d.GuildID, v.ID())
		if err != nil {
			slog.Error(err.Error())
		}
	}
}

type cmdHandler = func(context.Context, *events.ApplicationCommandInteractionCreate) error

func (d *DBot) CommandHandlers() map[string]cmdHandler {
	return map[string]cmdHandler{
		"play":            d.handlePlay,
		"wypierdalaj":     d.handleWypierdalaj,
		"set-bot-channel": d.handleMapChannel,
		"pause-play":      d.handlePause,
		"sound":           d.handleSound,
		"to-mp4":          d.handleToMP4,
		"save-playlist":   d.savePlaylist,
		"load-playlist":   d.playPlaylistFromDB,
	}
}
