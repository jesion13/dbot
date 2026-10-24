package dbot

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"dbot/pkg/config"
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

// Remapping the music or error channel redirects what the bot plays into and where it
// reports failures, so it is admin-only. Command registration is only a UI hint, so the
// handler re-checks.
func (d *DBot) handleMapChannel(ctx context.Context, e *events.ApplicationCommandInteractionCreate) error {
	if !isGuildAdmin(e) {
		return e.CreateMessage(discord.MessageCreate{
			Content: errNotAuthorized,
			Flags:   discord.MessageFlagEphemeral,
		})
	}

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

// handleJump plays an absolute position in the lineup. The panel numbers the queue
// absolutely, so the number the user read there is the number they type here.
func (d *DBot) handleJump(ctx context.Context, e *events.ApplicationCommandInteractionCreate) error {
	gid := e.GuildID()
	if gid == nil {
		return e.CreateMessage(discord.MessageCreate{
			Content: "jump only works in a server",
			Flags:   discord.MessageFlagEphemeral,
		})
	}
	// same rule as the panel buttons: only someone in the bot's own voice channel may
	// change what everyone hears
	if reason := d.voiceGuard(*gid, e.User().ID); reason != "" {
		return e.CreateMessage(discord.MessageCreate{Content: reason, Flags: discord.MessageFlagEphemeral})
	}

	position := e.SlashCommandInteractionData().Int("position")

	// atomic: the track is captured with the cursor move, so playback advancing on its
	// own cannot make the reply name the wrong one
	to, err := d.Player().JumpToTrack(position)
	if err != nil {
		return e.CreateMessage(discord.MessageCreate{
			Content: queueErrText(err, "could not jump"),
			Flags:   discord.MessageFlagEphemeral,
		})
	}

	// answered before the panel refresh: refreshGuild does REST calls and discord gives
	// a command three seconds
	reply := e.CreateMessage(discord.MessageCreate{
		Content: fmt.Sprintf("jumped to **%s**", displayTitle(to.Title)),
		Flags:   discord.MessageFlagEphemeral,
	})

	// show it on the panel now rather than at the next tick
	unlock := d.panels.lockUpdate(*gid)
	defer unlock()
	if err := d.refreshGuild(ctx, *gid); err != nil {
		slog.Error("player panel refresh after jump failed", slog.Any("error", err))
	}

	return reply
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
	// everything the user can pick about the output - area, trim, audio, format - is
	// picked on the edit page, so the command only says whether to open it
	edit := data.Bool("edit")

	var err error
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

	// the edit is picked later, in a browser, so its download must not live in the
	// per-command temp dir: that one is removed the moment this handler returns
	cropDir, cropOwned := "", false
	if edit {
		if config.PUBLIC_URL == "" {
			return errors.New("PUBLIC_URL is not set, so /to-mp4 edit:true has no link to hand out")
		}

		dir, err := os.MkdirTemp(config.TMP_PATH, "crop")
		if err != nil {
			return fmt.Errorf("failed creating crop dir: %w", err)
		}
		cropDir = dir
		ctx = context.WithValue(ctx, config.DirKey, dir)

		// the crop session takes this dir over once it exists; a command that fails before
		// that must not leave a whole download on disk
		defer func() {
			if !cropOwned {
				if err := os.RemoveAll(cropDir); err != nil {
					slog.Error("failed to remove a crop dir", slog.String("dir", cropDir), slog.Any("error", err))
				}
			}
		}()
	}

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
	if err != nil {
		return fmt.Errorf("failed getting file size: %w", err)
	}
	slog.Debug("handleToMP4", slog.String("file", f.Name()), slog.Int("size_KB", int(stat.Size()>>10)))

	videoInfo, err := ffmpeg.Probe(info.Filepath)
	if err != nil {
		return fmt.Errorf("probe downloaded video: %w", err)
	}
	hasHEVCVideo := ffmpeg.HasHEVCVideo(videoInfo)

	if edit {
		_, link, err := d.newCropSession(cropSessionParams{
			Dir:    cropDir,
			File:   info.Filepath,
			Info:   videoInfo,
			Target: transcodeTargetFor(e),
		})
		if err != nil {
			return err
		}
		cropOwned = true

		// the interaction token dies in about 15 minutes and the encode happens when the
		// user submits, so this is the last thing sent through the interaction
		msg := fmt.Sprintf("Pick what to keep: <%s>\nthe link works for %s", link, CropSessionTTL)
		_, err = d.Client.Rest.UpdateInteractionResponse(d.Client.ApplicationID, e.Token(),
			discord.MessageUpdate{Content: &msg})
		if err != nil {
			slog.Error("failed to answer with the crop link", slog.Any("error", err))
		}

		return ErrInteractionAnswered
	}

	// with no edit page nothing was asked of the file, so the only reason to re-encode
	// is that discord cannot stream it as it came
	if stat.Size() > ffmpeg.DiscordMaxFileSizeBytes || hasHEVCVideo {
		var reasons []string
		if stat.Size() > ffmpeg.DiscordMaxFileSizeBytes {
			reasons = append(reasons, "file too big")
		}
		if hasHEVCVideo {
			reasons = append(reasons, "HEVC video codec")
		}
		msg := "converting MP4: " + strings.Join(reasons, ", ")
		d.Client.Rest.UpdateInteractionResponse(d.Client.ApplicationID, e.Token(), discord.MessageUpdate{Content: &msg})

		f, err = ffmpeg.ToDiscordMP4(ctx, info.Filepath, false, ffmpeg.Clip{}, ffmpeg.Crop{})
		if err != nil {
			return fmt.Errorf("failed converting to MP4: %w", err)
		}
		defer f.Close()
	}

	// discord infers rendering from the extension
	return d.postTranscodedFile(ctx, transcodeTargetFor(e), "dupa.mp4", f)
}

// transcodeTargetFor is what a finished file should look like it came from, read off the
// interaction while it is still here.
func transcodeTargetFor(e *events.ApplicationCommandInteractionCreate) transcodeTarget {
	username := e.User().EffectiveName()
	if member := e.Member(); member != nil {
		username = member.EffectiveName()
	}

	return transcodeTarget{
		ChannelID: e.Channel().ID(),
		Username:  username,
		AvatarURL: e.User().EffectiveAvatarURL(),
	}
}

// webhookFor finds the webhook that lets us speak as the requester. A thread has no
// webhook of its own, so the hook lives on its parent channel while the message is sent
// with the thread id.
func (d *DBot) webhookFor(ctx context.Context, channelID snowflake.ID) (discord.IncomingWebhook, snowflake.ID, error) {
	channel, err := d.Client.Rest.GetChannel(channelID)
	if err != nil {
		return discord.IncomingWebhook{}, 0, fmt.Errorf("failed to get channel: %w", err)
	}

	webhookChannelID := channelID
	var threadID snowflake.ID
	if channel.Type() == discord.ChannelTypeGuildNewsThread ||
		channel.Type() == discord.ChannelTypeGuildPublicThread ||
		channel.Type() == discord.ChannelTypeGuildPrivateThread {
		threadID = channelID
		if thread, ok := channel.(discord.GuildThread); ok {
			if parentID := thread.ParentID(); parentID != nil {
				webhookChannelID = *parentID
			}
		}
	}

	hook, err := d.GetWebHook(ctx, webhookChannelID, DbotHook, "")
	if err != nil {
		return discord.IncomingWebhook{}, 0, fmt.Errorf("getWebhook: %w", err)
	}
	if hook == nil {
		return discord.IncomingWebhook{}, 0, errors.New("getWebhook: no webhook")
	}

	return *hook, threadID, nil
}

// postTranscodedFile delivers a finished file as the person who asked for it. The crop
// flow posts whenever the user feels like submitting, long after its interaction token
// expired, so neither of these touches the interaction.
func (d *DBot) postTranscodedFile(ctx context.Context, t transcodeTarget, fileName string, r io.Reader) error {
	hook, threadID, err := d.webhookFor(ctx, t.ChannelID)
	if err != nil {
		return err
	}

	_, err = d.Client.Rest.CreateWebhookMessage(hook.ID(), hook.Token,
		discord.WebhookMessageCreate{
			Username:  t.Username,
			AvatarURL: t.AvatarURL,
			Files:     []*discord.File{discord.NewFile(fileName, "", r)},
			Flags:     discord.MessageFlagSuppressEmbeds,
		},
		rest.CreateWebhookMessageParams{Wait: true, ThreadID: threadID},
	)

	return err
}

// postTranscodeNote is the text half of the same conversation: "cropping now" and the
// failure that replaced it.
func (d *DBot) postTranscodeNote(ctx context.Context, t transcodeTarget, content string) error {
	hook, threadID, err := d.webhookFor(ctx, t.ChannelID)
	if err != nil {
		return err
	}

	_, err = d.Client.Rest.CreateWebhookMessage(hook.ID(), hook.Token,
		discord.WebhookMessageCreate{
			Username:  t.Username,
			AvatarURL: t.AvatarURL,
			Content:   truncate(content),
		},
		rest.CreateWebhookMessageParams{Wait: true, ThreadID: threadID},
	)

	return err
}
