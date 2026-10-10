package dbot

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/fr-str/log"
)

type DBot struct {
	Ctx    context.Context
	Store  *store.Queries
	Backup *backup.Queries

	Client  *bot.Client
	GuildID snowflake.ID
	ytdlp.YTDLP
	// MusicPlayer is for plaing stuff from YT and others
	MusicPlayer *player.Player
}

func dbotErr(msg string, vars ...any) error {
	return fmt.Errorf("dbot: "+msg+": ", vars...)
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

func Start(ctx context.Context, client *bot.Client, dbstore *store.Queries) *DBot {
	backupDB, err := db.ConnectBackup(ctx, "backup.db", "")
	if err != nil {
		panic(err)
	}

	d := DBot{
		Ctx:         ctx,
		Client:      client,
		GuildID:     snowflake.MustParse(config.GUILD_ID),
		MusicPlayer: player.NewPlayer(),
		Store:       dbstore,
		Backup:      backupDB,
	}

	runner := startJobRunner(ctx, dbstore)
	runner.RegisterJob(DownloadJob, d.downloadAsync)
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
			log.Error(Err.Err.Error())
			ch, err := d.Store.GetChannel(ctx, store.GetChannelParams{
				Gid:  Err.GID,
				Type: musicChannel,
			})
			if err != nil {
				log.Error(err.Error())
				continue
			}

			chID := ch.Chid
			errChan, err := d.Store.GetChannel(ctx, store.GetChannelParams{
				Gid:  ch.Gid,
				Type: errorChannel,
			})
			log.Trace("errChan", log.JSON(errChan), log.Err(err))
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
				log.Error(err.Error())
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
	log.Trace("[dupa]", log.Any("channel.ChannelID", *vs.ChannelID))

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
	log.Trace("SaveSound", log.Any("params.Link", params.Link))

	aliases := strings.Split(params.Aliases, ",")

	f, err := d.downloadAsMP4(ctx, params.Link)
	if err != nil {
		return fmt.Errorf("failed to download file '%s': %w", params.Link, err)
	}

	name := fmt.Sprintf("%s.mp4", aliases[0])
	log.Trace("SaveSound", log.Any("link", name))
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
	log.Trace("SaveSound", log.JSON(sound))

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
	log.Info("ready")
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
	log.Trace("isValidUrl", log.JSON(u))
	return err == nil && u.Host != ""
}

func (d *DBot) play(gID, uID string, url string) error {
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

	case strings.Contains(url, "/playlist") || strings.Contains(url,"&list="):
		err := d.playFromYTPlaylist(url,gID)
		if err != nil {
			return fmt.Errorf("failed load playlist: %w", err)
		}

	default:
		art, err := d.Backup.GetArtefact(d.Ctx, url)
		if err != nil {
			go saveVideoInDir(d, url, gID, "play_history")
		} else {
			log.Trace("playHistory: artefact already exists")
			url = art.Path
		}

		d.MusicPlayer.Add(url)
	}

	return nil
}

func saveVideoInDir(d *DBot, url string, gID string, dir string) {
	log.Trace("saveInPlayHistory", log.String("url", url), log.String("gid", gID))
	meta := BackupFileParams{
		OriginUrl:   url,
		GID:         gID,
		Dirs:        dir,
		PrependTime: false,
		File:        nil,
	}

	jsonMeta, err := json.Marshal(meta)
	if err != nil {
		log.Error("lol dupa", log.Err(err), log.String("meta", fmt.Sprintf("%+v", meta)))
		return
	}

	_, err = d.Store.Enqueue(d.Ctx, store.EnqueueParams{
		Meta:    string(jsonMeta),
		JobType: BackupJob,
	})
	if err != nil {
		log.Error("lol dupa", log.Err(err), log.String("meta", fmt.Sprintf("%+v", meta)))
	}
}

func (d *DBot) searchAndPlay(url string) error {
	url = fmt.Sprintf(`ytsearch:"%s"`, url)
	d.MusicPlayer.Add(url)
	return nil
}

func (d *DBot) playFromYTPlaylist(url string,gID string) error {
	info, err := d.PlaylistInfo(url)
	if err != nil {
		return fmt.Errorf("failed getting playlist info: %w", err)
	}

	log.Trace("adding tracks from playlist", log.Int("len", len(info.Entries)))
	for i := range info.Entries {
		if info.Entries[i].Duration == nil {
			log.Trace(
				"skipping due to null duration, probably deleted vid",
				log.String("title", info.Entries[i].Title),
				log.String("url", info.Entries[i].URL),
			)
			continue
		}
		u := info.Entries[i].URL
		art, err := d.Backup.GetArtefact(d.Ctx, u)
		if err != nil {
			go saveVideoInDir(d, u, gID, "play_history")
		} else {
			u = art.Path
		}
		d.MusicPlayer.Add(u)
	}

	return nil
}

func (d *DBot) savePlaylistFromYT(name, url, gID string) error {
	info, err := d.PlaylistInfo(url)
	if err != nil {
		return fmt.Errorf("failed getting playlist info: %w", err)
	}
	if len(info.Entries) == 0 {
		return fmt.Errorf("failed to get video list from playlist")
	}

	playlist, err := d.Store.CreatePlaylist(d.Ctx, store.CreatePlaylistParams{
		GuildID: gID,
		Name:    name,
		YoutubeUrl: sql.NullString{
			String: info.WebpageURL,
			Valid:  true,
		},
	})
	if err != nil {
		return fmt.Errorf("failed creating playlist: %w", err)
	}

	for i := range info.Entries {
		if info.Entries[i].Duration == nil {
			log.Trace(
				"skipping due to null duration, probably deleted vid",
				log.String("title", info.Entries[i].Title),
				log.String("url", info.Entries[i].URL),
			)
			continue
		}

		meta := DownloadAsyncMeta{
			PlaylistID:  playlist.ID,
			URL:         info.Entries[i].URL,
			GID:         gID,
			Name:        info.Entries[i].Title,
			DownloadFor: "playlist_vids",
		}

		b, err := json.Marshal(meta)
		if err != nil {
			log.Error("lol dupa", log.Err(err), log.String("meta", fmt.Sprintf("%+v", meta)))
			continue
		}

		_, err = d.Store.Enqueue(d.Ctx, store.EnqueueParams{
			Meta:      string(b),
			FailCount: 0,
			Status:    "new",
			JobType:   DownloadJob,
		})
		if err != nil {
			log.Error("lol dupa", log.Err(err), log.String("meta", fmt.Sprintf("%+v", meta)))
			continue
		}
	}

	return nil
}

func (d *DBot) loadPlaylistFromDB(name string, gID string) error {
	playlist, err := d.Store.GetPlaylist(d.Ctx, store.GetPlaylistParams{
		GuildID: gID,
		Name:    name,
	})
	if err != nil {
		return fmt.Errorf("could not find playlist: %w", err)
	}

	list, err := d.Store.ListPlaylistEntries(d.Ctx, playlist.ID)
	if err != nil {
		return fmt.Errorf("could not find playlist: %w", err)
	}

	for _, elem := range list {
		d.MusicPlayer.Add(elem.Filepath)
	}

	return nil
}
