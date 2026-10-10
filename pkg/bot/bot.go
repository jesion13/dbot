package dbot

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"

	"dbot/pkg/backup"
	"dbot/pkg/config"
	"dbot/pkg/db"
	"dbot/pkg/ffmpeg"
	jobrunner "dbot/pkg/job_runner"
	"dbot/pkg/player"
	"dbot/pkg/store"
	"dbot/pkg/ytdlp"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/snowflake/v2"
)

type DBot struct {
	Ctx   context.Context
	Store *store.Queries
	// StoreDB is Store plus the connection, used for transactional snapshot writes.
	StoreDB *db.Store
	Backup  *backup.Queries

	Client  *bot.Client
	GuildID snowflake.ID
	ytdlp.YTDLP
	// PlaylistInfoFunc fetches playlist metadata; nil means use yt-dlp.
	PlaylistInfoFunc func(link string) (ytdlp.PlaylistMeta, error)
	// MusicPlayer is for plaing stuff from YT and others
	MusicPlayer *player.Player
}

// playlistInfo is the single place playlist metadata is fetched, so tests can stub it.
func (d *DBot) playlistInfo(link string) (ytdlp.PlaylistMeta, error) {
	if d.PlaylistInfoFunc != nil {
		return d.PlaylistInfoFunc(link)
	}
	return d.PlaylistInfo(link)
}

func dbotErr(msg string, vars ...any) error {
	return fmt.Errorf("dbot: "+msg, vars...)
}

var normalizeReplacer = strings.NewReplacer(
	"ą", "a",
	"ć", "c",
	"ę", "e",
	"ł", "l",
	"ż", "z",
	"ź", "z",
	"ó", "o",
	"ś", "s",
	"ń", "n",
	" ", "",
	"\n", "",
)

func normalize(s string) string {
	return normalizeReplacer.Replace(strings.ToLower(s))
}

func startJobRunner(ctx context.Context, db *store.Queries) jobrunner.Runner {
	runner := jobrunner.NewRunner(ctx, db)
	go runner.Loop()
	return runner
}

func Start(ctx context.Context, client *bot.Client, dbstore *db.Store) *DBot {
	backupDB, err := db.ConnectBackup(ctx, "backup.db", "")
	if err != nil {
		panic(err)
	}

	d := DBot{
		Ctx:         ctx,
		Client:      client,
		GuildID:     snowflake.MustParse(config.GUILD_ID),
		MusicPlayer: player.NewPlayer(),
		Store:       dbstore.Queries,
		StoreDB:     dbstore,
		Backup:      backupDB,
	}

	runner := startJobRunner(ctx, dbstore.Queries)
	runner.RegisterJob(BackupJob, d.backupVideoJob)

	// Listeners must be registered befor we open connection
	d.RegisterEventListiners()

	d.StartScheduler()
	d.interfaceLoop()

	err = client.OpenGateway(ctx)
	if err != nil {
		panic(err)
	}

	_, err = client.Rest.SetGuildCommands(client.ApplicationID, d.GuildID, cmds)
	if err != nil {
		panic(err)
	}

	go func() {
		for Err := range d.MusicPlayer.ErrChan {
			slog.Error(Err.Err.Error())
			ch, err := d.Store.GetChannel(ctx, store.GetChannelParams{
				Gid:  Err.GID,
				Type: musicChannel,
			})
			if err != nil {
				slog.Error(err.Error())
				continue
			}

			chID := ch.Chid
			errChan, err := d.Store.GetChannel(ctx, store.GetChannelParams{
				Gid:  ch.Gid,
				Type: errorChannel,
			})
			slog.Debug("errChan", slog.Any("errChan", errChan), slog.Any("error", err))
			if err == nil && errChan.Chid != "" && errChan.Chid != "0" {
				chID = errChan.Chid
			}

			msg := Err.Err.Error()
			switch {
			case errors.Is(Err.Err, ytdlp.ErrFailedToDownload):
				msg = "could not download video"
			case errors.Is(Err.Err, ffmpeg.ErrFfmpegError):
				msg = "failed converting to mp4"
			}

			_, err = d.Client.Rest.CreateMessage(snowflake.MustParse(chID), discord.MessageCreate{Content: msg})
			if err != nil {
				slog.Error(err.Error())
				continue
			}

		}
	}()
	return &d
}

var ErrVoiceDisabled = errors.New("voice disabled")

func (d *DBot) connectVoice(gID, uID snowflake.ID) error {
	// return ErrVoiceDisabled
	// skip if we are already connected
	if d.MusicPlayer.VC != nil {
		return nil
	}

	vs, err := d.getUserVC(gID, uID)
	if err != nil {
		return fmt.Errorf("failed to find User VC: %w", err)
	}

	vc := d.Client.VoiceManager.CreateConn(gID)
	openCtx, cancel := context.WithTimeout(d.Ctx, 10*time.Second)
	defer cancel()
	err = vc.Open(openCtx, *vs.ChannelID, false, false)
	if err != nil {
		return fmt.Errorf("failed to join VC: %w", err)
	}

	d.MusicPlayer.VC = vc
	d.MusicPlayer.VCID = *vs.ChannelID
	slog.Debug("[dupa]", slog.Any("channel.ChannelID", *vs.ChannelID))

	return nil
}

const (
	musicChannel = "music"
	errorChannel = "error"
	// adminChannel = "admin"
)

type SaveSoundParams struct {
	GID     string
	Aliases string
	Link    string
}

func (d *DBot) SaveSound(ctx context.Context, params SaveSoundParams) error {
	params.Aliases = normalize(params.Aliases)
	slog.Debug("SaveSound", slog.Any("params.Link", params.Link))

	aliases := strings.Split(params.Aliases, ",")

	f, err := d.downloadAsMP4(ctx, params.Link)
	if err != nil {
		return fmt.Errorf("failed to download file '%s': %w", params.Link, err)
	}

	name := fmt.Sprintf("%s.mp4", aliases[0])
	slog.Debug("SaveSound", slog.Any("link", name))
	file, err := d.backupFile(BackupFileParams{
		Name:        name,
		GID:         params.GID,
		Dirs:        "sounds",
		PrependTime: false,
		File:        f.File,
	})
	if err != nil {
		return fmt.Errorf("failed to backup file '%s': %w", params.Link, err)
	}

	sound, err := d.Store.AddSound(d.Ctx, store.AddSoundParams{
		Gid:       params.GID,
		Url:       file.Name,
		Aliases:   aliases,
		OriginUrl: params.Link,
	})
	if err != nil {
		return err
	}
	slog.Debug("SaveSound", slog.Any("sound", sound))

	srq.dropAndLoad(d.Store, sound.Gid)

	return nil
}

type MP4File struct {
	Filepath string
	Name     string
	File     *os.File
}

func (d *DBot) downloadAsMP4(ctx context.Context, url string) (MP4File, error) {
	vi, err := d.DownloadVideo(ctx, url)
	if err != nil {
		return MP4File{}, fmt.Errorf("'%s': %w", url, err)
	}

	f, err := ffmpeg.ConvertToMP4(ctx, vi.Filepath, ffmpeg.Clip{})
	if err != nil {
		return MP4File{}, fmt.Errorf("convertToMP4 failed '%s': %w", vi.Filepath, err)
	}

	return MP4File{
		Filepath: f.Name(),
		Name:     f.Name(),
		File:     f,
	}, nil
}

func (d *DBot) Ready(e *events.Ready) {
	slog.Info("ready")
}

// returns users VoiceState if user is connected to one
// errors if user is not in voice channel
func (d *DBot) getUserVC(gID, uID snowflake.ID) (discord.VoiceState, error) {
	vs, ok := d.Client.Caches.VoiceState(gID, uID)
	if !ok || vs.ChannelID == nil {
		return discord.VoiceState{}, fmt.Errorf("user <@%s> in guild '%s' is not in voice channel", uID, gID)
	}

	return vs, nil
}

func (d *DBot) mapChannel(params store.MapChannelParams) (store.Channel, error) {
	ch, err := d.Store.MapChannel(d.Ctx, params)
	if err != nil {
		return store.Channel{}, dbotErr("failed to save: %w", err)
	}

	return ch, nil
}

func (d *DBot) wypierdalajZVC() error {
	if d.MusicPlayer.VC != nil {
		d.MusicPlayer.VC.Close(d.Ctx)
	}
	d.MusicPlayer.Close()

	d.MusicPlayer = player.NewPlayer()
	return nil
}

func isValidUrl(s string) bool {
	u, err := url.Parse(s)
	slog.Debug("isValidUrl", slog.Any("u", u))
	return err == nil && u.Host != ""
}

func (d *DBot) play(ctx context.Context, gID, uID string, url string) error {
	err := d.connectVoice(snowflake.MustParse(gID), snowflake.MustParse(uID))
	if err != nil {
		return err
	}

	switch {
	case !isValidUrl(url):
		err := d.searchAndPlay(url)
		if err != nil {
			return fmt.Errorf("failed searching: %w", err)
		}

	default:
		link, ok := parseYTLink(url)
		if ok && link.Playlist {
			err := d.playPlaylistURL(ctx, gID, link.PlaylistID)
			if err != nil {
				return fmt.Errorf("failed load playlist: %w", err)
			}
			return nil
		}
		if ok {
			url = canonicalVideoURL(link.VideoID)
		}
		path, found, err := d.cachedVideo(ctx, "", url)
		if err != nil {
			slog.Error("artefact lookup failed", slog.Any("error", err), slog.String("url", url))
		} else if found {
			slog.Debug("play: artefact already exists", slog.String("path", path))
			url = path
		} else {
			slog.Debug("play: not cached, queueing source and scheduling backup", slog.String("url", url))
			go d.saveVideoInDir(url, gID, videoBackupDir)
		}

		d.MusicPlayer.Add(url)
	}

	return nil
}

// playPlaylistURL queues a playlist from a /play url. An unknown playlist is registered
// with its initial snapshot; a known one plays its saved snapshot without syncing.
func (d *DBot) playPlaylistURL(ctx context.Context, gID, playlistID string) error {
	pl, err := d.Store.GetPlaylistByYoutubeID(ctx, store.GetPlaylistByYoutubeIDParams{
		GuildID:           gID,
		YoutubePlaylistID: playlistID,
	})
	if err == nil {
		_, err = d.queueSnapshot(ctx, gID, pl.ID)
		return err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("lookup playlist: %w", err)
	}

	// first time this playlist is seen: register it and save the initial snapshot
	meta, err := d.playlistInfo(canonicalPlaylistURL(playlistID))
	if err != nil {
		return fmt.Errorf("failed getting playlist info: %w", err)
	}
	if meta.ID != playlistID {
		return fmt.Errorf("%w: got '%s', want '%s'", ErrPlaylistIDMismatch, meta.ID, playlistID)
	}
	pl, err = d.saveSnapshot(ctx, gID, meta, true)
	if err != nil {
		return err
	}

	_, err = d.queueSnapshot(ctx, gID, pl.ID)
	return err
}

func (d *DBot) searchAndPlay(url string) error {
	url = fmt.Sprintf(`ytsearch:"%s"`, url)
	d.MusicPlayer.Add(url)
	return nil
}
