package dbot

import (
	"context"
	"log/slog"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/omit"
)

// discord rejects a non-positive position in the client, so the handler only ever
// sees 1 or more.
var minJumpPosition = 1

// descriptions are required by discord
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
		Name:        "playlist",
		Description: "queue a saved playlist, optionally resyncing it from YouTube",
		Options: []discord.ApplicationCommandOption{
			discord.ApplicationCommandOptionString{
				Name:         "name",
				Description:  "playlist",
				Required:     true,
				Autocomplete: true,
			},
			discord.ApplicationCommandOptionBool{
				Name:        "update",
				Description: "sync the saved snapshot with YouTube before queueing",
				Required:    false,
			},
		},
	},
	discord.SlashCommandCreate{
		Name:        "playlist-delete",
		Description: "delete a saved playlist (cached videos are kept)",
		Options: []discord.ApplicationCommandOption{
			discord.ApplicationCommandOptionString{
				Name:         "name",
				Description:  "playlist",
				Required:     true,
				Autocomplete: true,
			},
		},
		// hides the command from non-admins in the client, but the handler re-checks
		DefaultMemberPermissions: omit.NewPtr(discord.PermissionAdministrator),
	},
	discord.SlashCommandCreate{
		Name:        "player",
		Description: "show the player panel in this channel",
	},
	discord.SlashCommandCreate{
		Name:        "queue",
		Description: "browse the whole queue",
	},
	discord.SlashCommandCreate{
		Name:        "jump",
		Description: "play a track by its position in the queue",
		Options: []discord.ApplicationCommandOption{
			discord.ApplicationCommandOptionInt{
				Name:        "position",
				Description: "absolute position, as shown by the player panel",
				Required:    true,
				MinValue:    &minJumpPosition,
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
				Name:        "edit",
				Description: "Pick the area, the trim, the audio and the format in a browser page first",
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
		"playlist":        d.handlePlaylist,
		"playlist-delete": d.handlePlaylistDelete,
		"wypierdalaj":     d.handleWypierdalaj,
		"set-bot-channel": d.handleMapChannel,
		"player":          d.handlePlayer,
		"jump":            d.handleJump,
		"queue":           d.handleQueue,
		"pause-play":      d.handlePause,
		"sound":           d.handleSound,
		"to-mp4":          d.handleToMP4,
	}
}
