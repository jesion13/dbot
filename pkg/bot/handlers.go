package dbot

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"dbot/pkg/ffmpeg"
	"dbot/pkg/store"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
)

func parseTime(s string) (time.Duration, error) {
	if strings.Contains(s, ":") {
		parts := strings.Split(s, ":")
		var total time.Duration
		for i, p := range parts {
			v, err := strconv.Atoi(p)
			if err != nil {
				return 0, err
			}
			switch len(parts) - i {
			case 3:
				total += time.Duration(v) * time.Hour
			case 2:
				total += time.Duration(v) * time.Minute
			case 1:
				total += time.Duration(v) * time.Second
			}
		}
		return total, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		d, err = time.ParseDuration(s + "s")
		if err != nil {
			return 0, err
		}
	}
	return d, nil
}

func (d *DBot) handlePlay(ctx context.Context, e *events.ApplicationCommandInteractionCreate) error {
	data := e.SlashCommandInteractionData()
	url := data.String("link")
	att, hasAtt := data.OptAttachment("file")
	if !hasAtt && url == "" {
		return errors.New("you need to provide link or attachment")
	}
	if hasAtt {
		url = att.URL
	}

	err := d.play(ctx, e.GuildID().String(), e.User().ID.String(), url)
	if err != nil {
		return err
	}

	return e.CreateMessage(discord.MessageCreate{
		Content: fmt.Sprintf("added %s", url),
	})
}

func (d *DBot) handlePlaylist(ctx context.Context, e *events.ApplicationCommandInteractionCreate) error {
	data := e.SlashCommandInteractionData()
	rawID := data.String("name")
	update := data.Bool("update")

	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil {
		return fmt.Errorf("unknown playlist '%s'", rawID)
	}

	gID := e.GuildID().String()
	pl, err := d.Store.GetPlaylist(ctx, store.GetPlaylistParams{ID: id, GuildID: gID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrPlaylistNotFound
		}
		return fmt.Errorf("lookup playlist: %w", err)
	}

	if update {
		pl, err = d.syncPlaylist(ctx, pl)
		if err != nil {
			// no stale-snapshot fallback: the user asked for a sync that failed
			return err
		}
	}

	err = d.connectVoice(snowflake.MustParse(gID), snowflake.MustParse(e.User().ID.String()))
	if err != nil {
		return err
	}

	queued, pending, unavailable, err := d.queueSnapshot(ctx, gID, pl.ID)
	if err != nil {
		return err
	}
	if queued == 0 {
		return ErrNoPlayableEntries
	}

	msg := fmt.Sprintf("queued %d entries from '%s'", queued, pl.Title)
	if pending > 0 {
		msg += fmt.Sprintf(", %d still downloading in background", pending)
	}
	if unavailable > 0 {
		msg += fmt.Sprintf(", %d unavailable", unavailable)
	}
	return e.CreateMessage(discord.MessageCreate{Content: msg})
}

func (d *DBot) handleWypierdalaj(ctx context.Context, e *events.ApplicationCommandInteractionCreate) error {
	e.CreateMessage(discord.MessageCreate{
		Content: "sadge",
		Flags:   discord.MessageFlagEphemeral,
	})

	d.wypierdalajZVC()
	return nil
}

func (d *DBot) handleMapChannel(ctx context.Context, e *events.ApplicationCommandInteractionCreate) error {
	data := e.SlashCommandInteractionData()
	channel := data.Channel("channel")

	ch, err := d.mapChannel(store.MapChannelParams{
		Gid:    e.GuildID().String(),
		Chid:   channel.ID.String(),
		ChName: channel.Name,
		Type:   data.String("type"),
	})
	if err != nil {
		return err
	}

	return e.CreateMessage(discord.MessageCreate{
		Content: fmt.Sprintf("Channel <#%s> will be used as '%s' channel", ch.Chid, ch.Type),
		Flags:   discord.MessageFlagEphemeral,
	})
}

func (d *DBot) handlePause(ctx context.Context, e *events.ApplicationCommandInteractionCreate) error {
	err := e.CreateMessage(discord.MessageCreate{
		Content: fmt.Sprintf("paused %v", !d.MusicPlayer.Playing.Load()),
		Flags:   discord.MessageFlagEphemeral,
	})
	d.MusicPlayer.PlayPause()
	return err
}

func (d *DBot) handleSound(ctx context.Context, e *events.ApplicationCommandInteractionCreate) error {
	data := e.SlashCommandInteractionData()
	opts := SaveSoundParams{
		Aliases: data.String("aliases"),
		Link:    strings.TrimSpace(data.String("link")),
	}

	att, hasAtt := data.OptAttachment("file")
	if !hasAtt && opts.Link == "" {
		return errors.New("you need to provide link or attachment")
	}
	if hasAtt {
		opts.Link = att.URL
	}
	slog.Debug("handleSounds", slog.Any("opts", opts))

	err := e.CreateMessage(discord.MessageCreate{
		Content: fmt.Sprintf("Saving sound '%s'", opts.Link),
	})
	if err != nil {
		return err
	}

	opts.GID = e.GuildID().String()
	err = d.SaveSound(ctx, opts)
	if err != nil {
		return err
	}

	msg := fmt.Sprintf("Saved sound '%s', '%s'", strings.Split(opts.Aliases, ",")[0], opts.Link)
	_, err = d.Client.Rest.UpdateInteractionResponse(d.Client.ApplicationID, e.Token(), discord.MessageUpdate{
		Content: &msg,
	})
	return err
}

func (d *DBot) handleToMP4(ctx context.Context, e *events.ApplicationCommandInteractionCreate) error {
	data := e.SlashCommandInteractionData()
	format := data.String("format")
	if format == "" {
		format = "mp4"
	}
	mute := data.Bool("mute")

	var err error
	clip := ffmpeg.Clip{}
	if start := data.String("start"); start != "" {
		clip.Start, err = parseTime(start)
		if err != nil {
			return fmt.Errorf("invalid start time '%s': %w", start, err)
		}
	}
	if end := data.String("end"); end != "" {
		clip.End, err = parseTime(end)
		if err != nil {
			return fmt.Errorf("invalid end time '%s': %w", end, err)
		}
	}
	if clip.Start > 0 && clip.End > 0 && clip.End <= clip.Start {
		return fmt.Errorf("end time must be after start time")
	}

	url := data.String("link")
	if att, ok := data.OptAttachment("file"); ok {
		url = att.URL
	}

	err = e.CreateMessage(discord.MessageCreate{
		Content: "Working on it, might take some time...",
		Flags:   discord.MessageFlagEphemeral,
	})
	if err != nil {
		return err
	}

	slog.Debug("handleToMP4", slog.String("url", url))

	info, err := d.DownloadVideoSmall(ctx, url)
	if err != nil {
		return fmt.Errorf("failed downloading video: %w", err)
	}

	// if download succeded backup file
	go d.saveVideoInDir(url, e.GuildID().String(), "tomp4")

	f, err := os.Open(info.Filepath)
	if err != nil {
		return fmt.Errorf("dupa")
	}
	defer f.Close()

	stat, err := f.Stat()
	slog.Debug("handleToMP4", slog.String("file", f.Name()), slog.Int("size_KB", int(stat.Size()>>10)))
	if err != nil {
		return fmt.Errorf("failed getting file size: %w", err)
	}

	videoInfo, err := ffmpeg.Probe(info.Filepath)
	if err != nil {
		return fmt.Errorf("probe downloaded video: %w", err)
	}
	hasHEVCVideo := ffmpeg.HasHEVCVideo(videoInfo)

	// Always convert GIFs and MP4s that exceed the upload limit, need edits, or use HEVC.
	if format == "gif" || stat.Size() > ffmpeg.DiscordMaxFileSizeBytes || mute || clip.Start > 0 || clip.End > 0 || hasHEVCVideo {
		if format == "gif" {
			msg := "converting to GIF..."
			d.Client.Rest.UpdateInteractionResponse(d.Client.ApplicationID, e.Token(), discord.MessageUpdate{Content: &msg})

			slog.Info("converting to GIF")

			f, err = ffmpeg.ToDiscordGIF(ctx, info.Filepath, clip)
			if err != nil {
				return fmt.Errorf("failed converting to GIF: %w", err)
			}
			defer f.Close()

			info, err := f.Stat()
			if err != nil {
				return err
			}

			if info.Size() >= ffmpeg.DiscordMaxFileSizeBytes {
				return errors.New("failed converting to GIF: file to big")
			}
		} else {
			var reasons []string
			if clip.Start > 0 || clip.End > 0 {
				reasons = append(reasons, "clipping")
			}
			if stat.Size() > ffmpeg.DiscordMaxFileSizeBytes {
				reasons = append(reasons, "file too big")
			}
			if mute {
				reasons = append(reasons, "mute requested")
			}
			if hasHEVCVideo {
				reasons = append(reasons, "HEVC video codec")
			}
			msg := "converting MP4: " + strings.Join(reasons, ", ")
			d.Client.Rest.UpdateInteractionResponse(d.Client.ApplicationID, e.Token(), discord.MessageUpdate{Content: &msg})

			f, err = ffmpeg.ToDiscordMP4(ctx, info.Filepath, mute, clip)
			if err != nil {
				return fmt.Errorf("failed converting to MP4: %w", err)
			}
			defer f.Close()
		}
	}

	channel, err := d.Client.Rest.GetChannel(e.Channel().ID())
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	webhookChannelID := e.Channel().ID()
	isThread := channel.Type() == discord.ChannelTypeGuildNewsThread ||
		channel.Type() == discord.ChannelTypeGuildPublicThread ||
		channel.Type() == discord.ChannelTypeGuildPrivateThread
	if isThread {
		if parentID := channel.(discord.GuildThread).ParentID(); parentID != nil {
			webhookChannelID = *parentID
		}
	}

	hook, err := d.GetWebHook(ctx, webhookChannelID, DbotHook, "")
	if err != nil {
		return fmt.Errorf("getWebhook: %w", err)
	}

	member := e.Member()
	username := e.User().EffectiveName()
	if member != nil {
		username = member.EffectiveName()
	}

	// Determine filename based on format; Discord infers rendering from the extension
	fileName := "dupa.mp4"
	if format == "gif" {
		fileName = "dupa.webp"
	}

	msgCreate := discord.WebhookMessageCreate{
		Username:  username,
		AvatarURL: e.User().EffectiveAvatarURL(),
		Files: []*discord.File{
			discord.NewFile(fileName, "", f),
		},
		Flags: discord.MessageFlagSuppressEmbeds,
	}

	params := rest.CreateWebhookMessageParams{Wait: true}
	if isThread {
		params.ThreadID = e.Channel().ID()
	}

	_, err = d.Client.Rest.CreateWebhookMessage(hook.ID(), hook.Token, msgCreate, params)
	return err
}
