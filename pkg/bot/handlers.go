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
	"unicode/utf8"

	"dbot/pkg/ffmpeg"
	"dbot/pkg/store"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
)

// The interaction was already acknowledged, so the generic error path in listeners.go
// must not send a second initial response. Returned even when delivering the final
// message failed: the interaction is consumed either way.
var ErrInteractionAnswered = errors.New("interaction already answered")

const maxMessageLen = 2000

func truncate(s string) string {
	if len(s) <= maxMessageLen {
		return s
	}
	s = s[:maxMessageLen-1]

	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// Edits the deferred "thinking" response first, falling back to a follow-up message.
func (d *DBot) answerDeferred(e *events.ApplicationCommandInteractionCreate, content string, ephemeral bool) error {
	followup := discord.MessageCreate{Content: content}
	if ephemeral {
		// a fresh follow-up needs the flag again, or a private confirmation goes public
		followup.Flags = discord.MessageFlagEphemeral
	}
	return deliverDeferred(content,
		func(c string) error {
			_, err := d.Client.Rest.UpdateInteractionResponse(d.Client.ApplicationID, e.Token(), discord.MessageUpdate{Content: &c})
			return err
		},
		func(c string) error {
			m := followup
			m.Content = c
			_, err := d.Client.Rest.CreateFollowupMessage(d.Client.ApplicationID, e.Token(), m)
			return err
		})
}

// Never sends another initial interaction response. edit and followup are injected so
// every delivery path is testable without discord.
func deliverDeferred(content string, edit, followup func(string) error) error {
	content = truncate(content)

	err := edit(content)
	if err == nil {
		return ErrInteractionAnswered
	}
	slog.Error("failed editing deferred response", slog.Any("error", err))

	if err := followup(content); err != nil {

		slog.Error("failed sending deferred follow-up", slog.Any("error", err))
	}
	return ErrInteractionAnswered
}

// Playlist playback always does queue/cache/voice work, so those are deferred.
func isPlaylistRequest(url string) bool {
	link, ok := parseYTLink(url)
	return ok && link.Playlist
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

	deferred := isPlaylistRequest(url)
	if deferred {
		if err := e.DeferCreateMessage(false); err != nil {
			return err
		}
	}

	err := d.play(ctx, e.GuildID().String(), e.User().ID.String(), url)
	if deferred {
		if err != nil {
			return d.answerDeferred(e, err.Error(), false)
		}
		return d.answerDeferred(e, fmt.Sprintf("added %s", url), false)
	}
	if err != nil {
		return err
	}

	return e.CreateMessage(discord.MessageCreate{
		Content: fmt.Sprintf("added %s", url),
	})
}

func (d *DBot) handlePlaylist(ctx context.Context, e *events.ApplicationCommandInteractionCreate) error {

	if err := e.DeferCreateMessage(false); err != nil {
		return err
	}
	answer := func(res queueResult, err error) error {
		if err != nil {
			return d.answerDeferred(e, err.Error(), false)
		}
		return d.answerDeferred(e, playlistQueuedMsg(res), false)
	}

	data := e.SlashCommandInteractionData()
	rawID := data.String("name")
	update := data.Bool("update")

	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil {
		return d.answerDeferred(e, fmt.Sprintf("unknown playlist '%s'", rawID), false)
	}

	gID := e.GuildID().String()
	pl, err := d.Store.GetPlaylist(ctx, store.GetPlaylistParams{ID: id, GuildID: gID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return d.answerDeferred(e, ErrPlaylistNotFound.Error(), false)
		}
		return d.answerDeferred(e, fmt.Sprintf("lookup playlist: %v", err), false)
	}

	if update {
		pl, err = d.syncPlaylist(ctx, pl)
		if err != nil {

			return d.answerDeferred(e, err.Error(), false)
		}
	}

	err = d.connectVoice(snowflake.MustParse(gID), snowflake.MustParse(e.User().ID.String()))
	if err != nil {
		return answer(queueResult{}, err)
	}

	res, err := d.queueSnapshot(ctx, gID, pl.ID)
	if err != nil {
		return answer(queueResult{}, err)
	}
	if res.queued == 0 {
		return d.answerDeferred(e, ErrNoPlayableEntries.Error(), false)
	}
	return answer(res, nil)
}

// "queued N" alone hides whether the media is local, still downloading, or could not
// be cached at all.
func playlistQueuedMsg(res queueResult) string {
	msg := fmt.Sprintf("queued %d entries", res.queued)
	if res.cached > 0 {
		msg += fmt.Sprintf(", %d from cache", res.cached)
	}
	if res.waiting > 0 {
		msg += fmt.Sprintf(", %d waiting for cache", res.waiting)
	}
	if res.unscheduled > 0 {
		msg += fmt.Sprintf(", %d could not be cached", res.unscheduled)
	}
	if res.unavailable > 0 {
		msg += fmt.Sprintf(", %d unavailable", res.unavailable)
	}
	return msg
}

const errNotAuthorized = "you need to be a server administrator"

// Command registration is only a UI hint, so it can never authorize a mutation.
func isGuildAdmin(e *events.ApplicationCommandInteractionCreate) bool {
	if e.GuildID() == nil {
		return false
	}
	member := e.Member()
	if member == nil {
		return false
	}
	return member.Permissions.Has(discord.PermissionAdministrator)
}

func (d *DBot) handlePlaylistDelete(ctx context.Context, e *events.ApplicationCommandInteractionCreate) error {

	if err := e.DeferCreateMessage(true); err != nil {
		return err
	}

	if !isGuildAdmin(e) {
		return d.answerDeferred(e, errNotAuthorized, true)
	}

	id, err := strconv.ParseInt(e.SlashCommandInteractionData().String("name"), 10, 64)
	if err != nil {
		return d.answerDeferred(e, "unknown playlist", true)
	}

	title, err := d.deletePlaylist(ctx, id, e.GuildID().String())
	if err != nil {
		if errors.Is(err, ErrPlaylistNotFound) || errors.Is(err, sql.ErrNoRows) {

			return d.answerDeferred(e, "playlist not found", true)
		}
		return d.answerDeferred(e, "could not delete playlist", true)
	}

	return d.answerDeferred(e, fmt.Sprintf("Deleted playlist %q. Cached videos were kept.", title), true)
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
	err := d.Player().TogglePause()
	if err != nil {
		return err
	}
	return e.CreateMessage(discord.MessageCreate{
		Content: fmt.Sprintf("paused %v", d.Player().Paused()),
		Flags:   discord.MessageFlagEphemeral,
	})
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
		clip.Start, err = ffmpeg.ParseTime(start)
		if err != nil {
			return fmt.Errorf("invalid start time '%s': %w", start, err)
		}
	}
	if end := data.String("end"); end != "" {
		clip.End, err = ffmpeg.ParseTime(end)
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

	// discord infers rendering from the extension
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
