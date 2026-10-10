package dbot

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"dbot/pkg/backup"
	"dbot/pkg/config"
	"dbot/pkg/ffmpeg"
	"dbot/pkg/ytdlp"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"log/slog"
)

func (d *DBot) RegisterEventListiners() {
	cmdHandlers := d.CommandHandlers()
	d.Client.AddEventListeners(
		bot.NewListenerFunc(d.commands(cmdHandlers)),
		bot.NewListenerFunc(d.Ready),
		bot.NewListenerFunc(d.messagesListener),
		bot.NewListenerFunc(d.messagesEditListener),
		bot.NewListenerFunc(d.onUserVoiceStateChange),
		bot.NewListenerFunc(d.messageDeleteListener),
	)
}

func createContextTmpDir(ctx context.Context) context.Context {
	dir, err := os.MkdirTemp(config.TMP_PATH, strconv.Itoa(int(time.Now().UnixNano())))
	if err != nil {
		slog.Error("dupa", slog.Any("error", err))
	}

	ctx = context.WithValue(ctx, config.DirKey, dir)
	go func() {
		<-ctx.Done()
		os.RemoveAll(dir)
	}()
	return ctx
}

func (d *DBot) commands(cmdHandlers map[string]cmdHandler) func(e *events.ApplicationCommandInteractionCreate) {
	return func(e *events.ApplicationCommandInteractionCreate) {
		name := e.SlashCommandInteractionData().CommandName()
		if h, ok := cmdHandlers[name]; ok {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx = createContextTmpDir(ctx)

			err := h(ctx, e)
			if err != nil {
				if errors.Is(err, ErrVoiceDisabled) {
					e.CreateMessage(discord.MessageCreate{
						Content: "Voice is disabled",
						Flags:   discord.MessageFlagEphemeral,
					})
					return
				}
				slog.Error("uuu duuuuuuuuuuuuuuuuuupa", slog.Any("error", err), slog.String("cmd", name), slog.String("err_type", fmt.Sprintf("%T", err)))

				msg := err.Error()
				if errors.Is(err, ytdlp.ErrFailedToDownload) {
					msg = "could not download video"
				}
				if len(msg) > 2000 {
					msg = msg[:2000-1]
				}
				_, err := d.Client.Rest.CreateMessage(e.Channel().ID(), discord.MessageCreate{Content: msg})
				if err != nil {
					slog.Error("response failed", slog.Any("error", err))
				}
			}
		}
	}
}

func (d *DBot) messagesEditListener(e *events.GuildMessageUpdate) {
	if e.Message.Author.Bot {
		return
	}

	// err := updateBackupMessage(d, e.Message)
	// if err != nil {
	// 	return err
	// }
}

func (d *DBot) messageDeleteListener(e *events.GuildMessageDelete) {
	err := d.Backup.DeleteArtefact(d.Ctx, backup.DeleteArtefactParams{
		Gid:   e.GuildID.String(),
		Chid:  e.ChannelID.String(),
		Msgid: e.MessageID.String(),
	})
	if err != nil {
		slog.Error("failed to delete artefact", slog.Any("error", err))
	}
}

func (d *DBot) messagesListener(e *events.GuildMessageCreate) {
	m := e.Message
	info, _, err := BackupAttachment(d, m)
	if err == nil && info.PossibleDupe != nil {
		msg, err := d.Client.Rest.CreateMessage(m.ChannelID, discord.MessageCreate{Content: fmt.Sprintf(
			"Możliwy duplikat https://discord.com/channels/%s/%s/%s",
			info.PossibleDupe.Gid,
			info.PossibleDupe.Chid,
			info.PossibleDupe.Msgid,
		)})
		if err == nil {
			go func() {
				time.Sleep(60 * time.Second)
				d.Client.Rest.DeleteMessage(msg.ChannelID, msg.ID)
			}()
		}
	}
	if m.Author.Bot {
		return
	}
	// err := backupMessage(d, m)
	// if err != nil {
	// 	return err
	// }

	isKnownSound(d, m)
	soundAll(d, m)
	// transcodeToh264(d, m)
}

func transcodeToh264(d *DBot, m discord.Message) {
	if len(m.Attachments) == 0 {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	ctx = createContextTmpDir(ctx)
	defer cancel()
	badCodecs := []string{
		"hevc",
		"av1",
		"h265",
	}

	for _, att := range m.Attachments {
		if att.ContentType == nil || !strings.Contains(*att.ContentType, "video") {
			continue
		}
		meta, err := d.DownloadVideo(ctx, att.URL)
		if err != nil {
			slog.Error(err.Error())
			continue
		}

		videoInfo, err := ffmpeg.Probe(meta.Filepath)
		if err != nil {
			slog.Error(err.Error())
			continue
		}
		for i := range videoInfo.Streams {
			s := &videoInfo.Streams[i]
			if s.CodecType != "video" {
				continue
			}

			if !slices.Contains(badCodecs, s.CodecName) {
				continue
			}

			d.Client.Rest.AddReaction(m.ChannelID, m.ID, "bosy:1220157705273086002")

			// transcode and upload
			f, err := ffmpeg.ConvertToMP4(ctx, meta.Filepath, ffmpeg.Clip{})
			if err != nil {
				slog.Error("transcodeAndReupload", slog.Any("error", err))
				return
			}
			defer f.Close()
			defer os.Remove(f.Name())
			msg, err := d.Client.Rest.CreateMessage(m.ChannelID, discord.MessageCreate{
				Files: []*discord.File{
					discord.NewFile("dupa.mp4", "", f),
				},
			})
			if err != nil {
				slog.Error("transcodeAndReupload", slog.Any("error", err))
				return
			}
			d.Client.Rest.AddReaction(msg.ChannelID, msg.ID, "skipper:1330256451331031162")
		}
	}
}

func soundAll(d *DBot, m discord.Message) {
	normalized := normalize(m.Content)
	if normalized != "sound-all" && normalized != "event-all" {
		return
	}

	slog.Debug("sound all", slog.String("msg", normalized))

	gID := ""
	if m.GuildID != nil {
		gID = m.GuildID.String()
	}

	sounds, err := d.Store.SelectSounds(d.Ctx, gID)
	if err != nil {
		slog.Error(err.Error())
		return
	}

	rand.Shuffle(len(sounds), func(i, j int) {
		sounds[i], sounds[j] = sounds[j], sounds[i]
	})

	if m.GuildID == nil {
		return
	}
	err = d.connectVoice(*m.GuildID, m.Author.ID)
	if err != nil {
		slog.Error(err.Error())
		return
	}

	for _, s := range sounds {
		d.MusicPlayer.PlaySound(s.Url)
	}
}

func isKnownSound(d *DBot, m discord.Message) {
	msg := m.Content
	normalized := normalize(m.Content)
	slog.Debug("isKnownSound", slog.String("msg", msg), slog.String("normalized", normalized))

	gID := ""
	if m.GuildID != nil {
		gID = m.GuildID.String()
	}

	sound, err := findSound(d.Store, normalized, gID)
	if err != nil {
		if !errors.Is(err, ErrSoundNotFound) {
			slog.Info("failed to find sound", slog.Any("error", err))
		}
		return
	}

	if m.GuildID == nil {
		return
	}
	err = d.connectVoice(*m.GuildID, m.Author.ID)
	if err != nil {
		if errors.Is(err, ErrVoiceDisabled) && m.ChannelID.String() == "629733112074600469" {
			d.Client.Rest.CreateMessage(m.ChannelID, discord.MessageCreate{Content: "Voice is disabled"})
			return
		}
		slog.Error(err.Error())
		return
	}

	for _, s := range sound {
		if strings.Contains(s.Url, "riotgames") {
			continue
		}
		slog.Debug("isKnownSound", slog.Any("sound.Url", s.Url), slog.Any("name", s.Aliases))
		d.MusicPlayer.PlaySound(s.Url)
	}
}

func (d *DBot) onUserVoiceStateChange(e *events.GuildVoiceStateUpdate) {
	slog.Debug("onUserVoiceStateChange", slog.Any("d.MusicPlayer.VC == nil", d.MusicPlayer.VC == nil))
	if d.MusicPlayer.VC == nil {
		return
	}

	botChanID := d.MusicPlayer.VCID
	slog.Debug("onUserVoiceStateChange", slog.Any("botChanID", botChanID))
	gID := e.VoiceState.GuildID
	var botChanUserCount int
	for vs := range d.Client.Caches.VoiceStates(gID) {
		if member, ok := d.Client.Caches.Member(gID, vs.UserID); ok && member.User.Bot {
			continue
		}
		if vs.ChannelID == nil || *vs.ChannelID != botChanID {
			continue
		}

		botChanUserCount++
	}

	if botChanUserCount > 0 {
		return
	}

	d.wypierdalajZVC()
}
