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
	"sync"
	"sync/atomic"
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
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
)

type DBot struct {
	Ctx   context.Context
	Store *store.Queries
	// Store plus the connection, for transactional snapshot writes
	StoreDB *db.Store
	Backup  *backup.Queries

	Client  *bot.Client
	GuildID snowflake.ID
	ytdlp.YTDLP
	// PlaylistInfoFunc fetches playlist metadata; nil means use yt-dlp.
	PlaylistInfoFunc func(link string) (ytdlp.PlaylistMeta, error)
	// playerMu guards the pointer only, and is never held across a Close, which can
	// take a while

	playerMu    sync.RWMutex
	MusicPlayer *player.Player
	// two disconnect events arriving at once would otherwise both shut down the same
	// player, close its connection and install two replacements, orphaning one

	voiceMu sync.Mutex
	// belongs to the bot and outlives every player, so a swap can never silence
	// reporting; players never close it
	playerErrs chan player.Err
	// lets a test assert that retired players leave no forwarder behind
	playerForwarders atomic.Int32
	// panels holds the one live player panel per guild; a replaced panel is dropped
	// from here, so its buttons stop working
	panels  panelRegistry
	lastSig string

	// discord seams for the panel, replaced in tests
	answerPanel   func(snowflake.ID, string, discord.MessageCreate) (snowflake.ID, error)
	createPanel   func(context.Context, snowflake.ID, discord.MessageCreate) (snowflake.ID, error)
	deletePanel   func(channelID, messageID snowflake.ID) error
	editPanel     func(context.Context, snowflake.ID, snowflake.ID, discord.MessageUpdate) error
	followupPanel func(token string, msg discord.MessageCreate) error
	// newestPanel reports whether a message is still the last one in its channel
	newestPanel func(channelID, messageID snowflake.ID) (newest bool, err error)

	// the panel's own bookkeeping, bound to the store at startup so the active
	// message survives a restart
	loadPanelRow     func(context.Context, snowflake.ID) (storedPanel, error)
	rememberPanelRow func(context.Context, snowflake.ID, panelEntry) error
	forgetPanelRow   func(context.Context, snowflake.ID) error
}

// Never nil: tests build a DBot by hand, and selecting on a nil context panics.
func (d *DBot) botCtx() context.Context {
	if d.Ctx == nil {
		return context.Background()
	}
	return d.Ctx
}

// Safe against a concurrent replacement.
func (d *DBot) Player() *player.Player {
	d.playerMu.RLock()
	defer d.playerMu.RUnlock()
	return d.MusicPlayer
}

// Installs p and starts forwarding its errors. This is the low-level pointer setter
// and tears nothing down; use ReplacePlayer for a real transition.
func (d *DBot) SetPlayer(p *player.Player) {
	d.playerMu.Lock()
	d.MusicPlayer = p
	d.playerMu.Unlock()

	// exits when that player's ErrChan closes (once its workers are gone) or the bot
	// shuts down, so replacements never accumulate goroutines
	d.playerForwarders.Add(1)
	go func() {
		defer d.playerForwarders.Add(-1)
		for {
			select {
			case e, ok := <-p.ErrChan:
				if !ok {
					slog.Debug("player error forwarding stopped: player closed")
					return
				}
				d.forwardPlayerErr(e)
			case <-d.botCtx().Done():
				slog.Debug("player error forwarding stopped: bot shutting down")
				return
			}
		}
	}()
}

// The stream is bounded: a full one drops with a log rather than blocking a playback
// worker forever, because a stuck worker is worse than a lost message.
func (d *DBot) forwardPlayerErr(e player.Err) {
	if d.playerErrs == nil {
		return
	}
	select {
	case d.playerErrs <- e:
	default:
		slog.Error("player error stream full, dropping", slog.Any("error", e.Err))
	}
}

// One whole transition under one lock: shut the current player down, wait for it,
// close its connection only then, install exactly one replacement. Serialized, so a
// second transition cannot orphan the first one's replacement.
func (d *DBot) ReplacePlayer() (*player.Player, error) {
	d.voiceMu.Lock()
	defer d.voiceMu.Unlock()
	return d.replacePlayerLocked()
}

// ReplacePlayer's body. The caller must hold voiceMu.
func (d *DBot) replacePlayerLocked() (*player.Player, error) {
	old := d.Player()
	conn, _ := old.Conn()

	// Close joins the workers writing to the connection, so it must finish first.
	// playerMu is deliberately not held here.
	var closeErr error
	if err := old.Close(); err != nil {
		closeErr = fmt.Errorf("close player: %w", err)
		slog.Error("failed closing player", slog.Any("error", err))
	}
	if conn != nil {
		conn.Close(d.botCtx())
	}

	next := player.NewPlayer()
	d.SetPlayer(next)
	return next, closeErr
}

// Shares voiceMu with ReplacePlayer, so a connection can never be attached to a player
// a replacement has already snapshotted. A refused connection is closed here rather
// than left unowned.
func (d *DBot) AttachVoiceConnection(conn voice.Conn, vcid snowflake.ID) error {
	d.voiceMu.Lock()
	defer d.voiceMu.Unlock()

	if err := d.Player().SetVC(conn, vcid); err != nil {
		conn.Close(d.botCtx())
		return fmt.Errorf("attach voice connection: %w", err)
	}
	return nil
}

// The supported way to change connection: the old player and its connection are shut
// down first, so no worker ever writes to a connection that is being taken away.
func (d *DBot) Reconnect(conn voice.Conn, vcid snowflake.ID) (*player.Player, error) {
	d.voiceMu.Lock()
	defer d.voiceMu.Unlock()

	next, err := d.replacePlayerLocked()
	if err != nil {
		// the replacement still exists, so the new connection is not orphaned
		conn.Close(d.botCtx())
		return next, err
	}
	if err := next.SetVC(conn, vcid); err != nil {
		conn.Close(d.botCtx())
		return nil, fmt.Errorf("attach voice connection: %w", err)
	}
	return next, nil
}

// The single place playlist metadata is fetched, so tests can stub it.
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
		Ctx:        ctx,
		Client:     client,
		GuildID:    snowflake.MustParse(config.GUILD_ID),
		Store:      dbstore.Queries,
		StoreDB:    dbstore,
		Backup:     backupDB,
		playerErrs: make(chan player.Err, 16),
	}
	d.SetPlayer(player.NewPlayer())

	runner := startJobRunner(ctx, dbstore.Queries)
	runner.RegisterJob(BackupJob, d.backupVideoJob)

	// Listeners must be registered befor we open connection
	d.RegisterEventListiners()

	d.StartScheduler()

	err = client.OpenGateway(ctx)
	if err != nil {
		panic(err)
	}

	_, err = client.Rest.SetGuildCommands(client.ApplicationID, d.GuildID, cmds)
	if err != nil {
		panic(err)
	}

	go d.reportPlayerErrors(ctx)
	d.bindPanelStore(ctx)
	go d.cleanupOrphanPanelOnce(ctx)
	go d.ensurePanel(ctx)
	return &d
}

// reportPlayerErrors posts player failures to the guild's channel. It reads the
// bot's single stream, not any one player's channel, so replacing the player does
// not silence reporting.
func (d *DBot) reportPlayerErrors(ctx context.Context) {
	d.consumePlayerErrors(ctx, func(Err player.Err) {
		slog.Error(Err.Err.Error())
		ch, err := d.Store.GetChannel(ctx, store.GetChannelParams{
			Gid:  Err.GID,
			Type: musicChannel,
		})
		if err != nil {
			slog.Error(err.Error())
			return
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
		}
	})
}

// Returns on context cancellation: the stream belongs to the bot and is never closed,
// so a bare range would block forever.
func (d *DBot) consumePlayerErrors(ctx context.Context, handle func(player.Err)) {
	for {
		select {
		case Err := <-d.playerErrs:
			handle(Err)
		case <-ctx.Done():
			return
		}
	}
}

var ErrVoiceDisabled = errors.New("voice disabled")

func (d *DBot) connectVoice(gID, uID snowflake.ID) error {
	// return ErrVoiceDisabled
	// skip if we are already connected
	if conn, _ := d.Player().Conn(); conn != nil {
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

	if err := d.AttachVoiceConnection(vc, *vs.ChannelID); err != nil {
		return err
	}
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
	// Title and ID are what yt-dlp reported, before any filename trimming could have
	// touched them. Both are needed after the download: the id to name the stored file
	// and the title to record in the database.
	Title string
	ID    string
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
		Title:    vi.Title,
		ID:       vi.ID,
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

// wypierdalajZVC tears down the current voice session and installs a fresh player.
// It is the only voice-replacement entry point, so every path (the command, the
// voice-state listener, a reconnect) gets the same ordering and serialization.
func (d *DBot) wypierdalajZVC() error {
	_, err := d.ReplacePlayer()
	return err
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
		art, found, err := d.cachedVideo(ctx, "", url)
		if err != nil {
			slog.Error("artefact lookup failed", slog.Any("error", err), slog.String("url", url))
		} else if found {
			slog.Debug("play: artefact already exists", slog.String("path", art.Path))
			// url stays the source, filepath the local copy - the player never guesses
			return d.Player().Enqueue(cachedTrack(url, art))
		} else {
			slog.Debug("play: not cached, queueing source and scheduling backup", slog.String("url", url))
			go d.saveVideoInDir(url, gID, videoBackupDir)
		}

		return d.Player().Enqueue(player.Track{URL: url})
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
	return d.Player().Enqueue(player.Track{URL: url})
}
