# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

dbot is a Go Discord bot (module `dbot`, go 1.26) for a single guild: plays music/sounds from YouTube, records a backup of guild attachments/messages with perceptual-hash dedupe, transcodes video for Discord's upload limits, and keeps snapshots of YouTube playlists.

**Branch state**: `master` still uses `github.com/bwmarrin/discordgo` (via a `replace` to a private fork, `github.com/yeongaori/discordgo`, that adds real DAVE/E2EE voice support upstream discordgo lacks). Branch `rewrite-to-disgo` has migrated the whole bot to `github.com/disgoorg/disgo` + real libdave-backed DAVE encryption. Check `git branch --show-current` before assuming which API surface (`discordgo` vs `disgo`) a file uses — don't assume `master`'s shape when editing the migration branch, or vice versa.

## Build & dev commands

```bash
air                                            # hot reload (pkg/bot/bot.go etc. rebuild on save)
go build -tags 'debug' ./cmd/dbot              # enables pkg/dbg.Assert (no-op without the tag)
go build ./cmd/dbot                            # normal build
make build                                     # go mod tidy && docker build -t dbot .
```

On the `rewrite-to-disgo` branch (or any branch depending on `github.com/disgoorg/godave/libdave`), builds require CGO and Discord's native `libdave` shared library:
```bash
make libdave                                   # installs libdave for local dev (Arch; needs sudo pacman)
export PKG_CONFIG_PATH="$HOME/.local/lib/pkgconfig:$PKG_CONFIG_PATH"
export LD_LIBRARY_PATH="$HOME/.local/lib:$LD_LIBRARY_PATH"
CGO_ENABLED=1 go build ./...
```
`CGO_ENABLED=0` fails to link `golibdave`/`libdave` on that branch — this is expected. The Dockerfile's build stage installs libdave itself via the same install script before compiling.

### Tests
```bash
go test ./...                      # pkg/player holds no tests (the old hanging TestPlaylist is gone)
go test -race ./...                # needs libdave's pkg-config, see above
go test -cover ./...
```

### Lint / codegen
```bash
go vet ./...
go fmt ./...
go tool goose ...                  # migration tool, works via go.mod `tool` directive
sqlc generate                      # works with sqlc v1.27.0 on this toolchain.
                                    # Migrations are up-only by convention - a `-- +goose down` section
                                    # gets replayed as if it were up and corrupts the generated schema
```

## Architecture

### Three independent SQLite databases
Configured in one `sqlc.yaml` (3 separate `sql:` entries) but each is its own file, schema, and generated Go package — there is no cross-DB joining:
- **store** (`pkg/store`, schema `db/migrations/`, queries `db/queries/*.sql`) — main app data: channel mappings, sounds, playlists + playlist entries, the job queue table. Opened via `db.ConnectStore` at `$DB_DIR/<filename>`, which returns `*db.Store` — `store.Queries` embedded plus the `*sql.DB`, so `WithTx` can run a transaction (the DB has `MaxOpenConns(1)`: only ever use the tx-bound queries inside a transaction).
- **backup** (`pkg/backup`, schema `db/backup/`, queries `db/backup-queries/*.sql`) — attachment/message backup + perceptual-hash dedupe records. Opened via `db.ConnectBackup` at `$BACKUP_DIR/<filename>`.
- **cache** (`pkg/cache`, schema `db/cache/`, queries `db/cache-queries/*.sql`) — audio cache. Opened via `db.ConnectAudioCache` at `$TMP_PATH/<filename>`.

All three connection functions live in `pkg/db/db.go`, each: opens with `modernc.org/sqlite`, runs goose migrations embedded via `//go:embed` in the top-level `db` package, sets `PRAGMA journal_mode=WAL` (store/backup only — cache DB skips both pragmas), and caps `SetMaxOpenConns(1)` (SQLite + WAL, single-writer). Note: the second pragma is written as `PRAGMA synchronus=NORMAL` (typo, missing the second "o") in both call sites — SQLite silently ignores unknown pragmas, so this has never actually taken effect; don't assume `synchronous=NORMAL` is really set.

sqlc output is generated code — never hand-edit `pkg/store`, `pkg/backup`, `pkg/cache`. Edit the `.sql` files under `db/` and `db/*-queries/`, then regenerate.

### Job queue (`pkg/job_runner`)
A single polling loop (`Runner.Loop`), not a worker pool: pulls one row at a time from `store.NextInQueue`, dispatches by `JobType` string key to a handler registered via `RegisterJob`, writes status back (`done`/`failing` + `FailCount`), sleeps 5s on empty queue or error. One job type is registered in `pkg/bot/bot.go`'s `Start`: `BackupJob` (handler `backupVideoJob` in `pkg/bot/jobs.go`), which re-checks the shared artefact index before downloading so duplicate jobs are cheap no-ops.

### Player (`pkg/player`)
Two independent goroutine loops started in `NewPlayer()`:
- `musicLoop` — consumes `list.nextAudio`, plays full tracks via `playV2`, respects pause/queue state (`list` in `playlist.go`).
- `soundLoop` — consumes `queue`, plays short sounds via `playSound`, auto-pauses music while a sound plays.

Both pull audio via `ffmpeg` (opus-encoded OGG over a pipe) and parse frames with `pion/opus/pkg/oggreader`. On the `disgo` branch, frames are sent via `voice.Conn.SetSpeaking` + `UDP().Write(frame)` directly, paced to real 20ms/frame by a hand-rolled `framePacer` in `player.go` — **do not** switch this to disgo's `AudioSender`/`SetOpusFrameProvider`. That API assumes exclusive ownership of the `Conn`, but music and sounds are two independent producers sharing one `Conn` (a sound interrupts a song mid-stream); handing the Conn to a second provider silently abandons the first one's sender goroutine, which caused songs to never resume after a sound played. `playV2` holds `Player`'s own mutex per-frame-write so `PlayPause()` can gate playback mid-stream; `playSound` doesn't need that since sounds aren't pausable.

`PlayPause()` toggles `Player`'s embedded `sync.Mutex` directly as a gate — frame production blocks while paused. This only works because `Player` is also passed as the `sync.Locker` into the OGG frame provider.

### Discord event dispatch (disgo branch)
disgo defaults to **synchronous** event dispatch (one gateway-reader goroutine runs every listener inline). discordgo always dispatched handlers in their own goroutine. The migration must set `disgobot.WithEventManagerConfigOpts(disgobot.WithAsyncEventsEnabled())` in `cmd/dbot/main.go` — without it, any handler that blocks waiting on a gateway event it needs (e.g. `connectVoice` waiting on `VOICE_STATE_UPDATE`/`VOICE_SERVER_UPDATE` to arrive) deadlocks the entire bot.

### Logging (`pkg/logger`)
This project uses plain `log/slog` directly — no wrapper package, no `fr-str/log` dependency (removed). Call sites use `slog.Debug`/`slog.Info`/`slog.Error`/`slog.*Context` and `slog.String`/`slog.Int`/`slog.Any`/etc straight from the stdlib; there is no custom `log.Trace` level — it was merged into `Debug`. `pkg/logger.Handler` is the only custom piece: a `slog.Handler` rendering
```
HH:MM:SS.mmm LEVEL pkg/file.go:line message
   Key=value
   Struct.Field=value
```
flattening struct-valued attrs into dotted `Key.Field=value` lines (recursing into nested structs, respecting `error`/`fmt.Stringer` implementations before reflecting into a value's fields — see `pkg/logger/handler.go` if extending it). `cmd/dbot/main.go` calls `slog.SetDefault(logger.New(os.Stdout, logger.ParseLevel(config.LOG_LEVEL)))` once at startup (after `config.Load()`) and passes `slog.Default()` to `disgobot.WithLogger(...)`, so dbot's own logs and disgo's internal logs (gateway, voice, REST) render through the same handler/format.

`pkg/db`'s per-query SQL logging (`pkg/db/db.go`'s `logger()` helper) uses `logger.LevelSQL` (`slog.LevelDebug - 4`, rendered as `SQL`), one step below `Debug` — set `LOG_LEVEL=sql` to see it; the default `LOG_LEVEL=debug` hides SQL query spam but keeps everything else at Debug.

### Backup & dedupe (`pkg/bot/backup.go`)
`BackupAttachment` downloads every message attachment, computes a perceptual image hash (`goimagehash.DifferenceHash`, PNG/JPEG/WEBP only) and checks it against prior hashes in the backup DB (`checkHashAgainstDB`, paginated, distance ≤ 3 = dupe) before storing. Files land under `$BACKUP_DIR/<guild_id>/<dirs>/<name>` and `artefacts.path` is stored **relative** to `$BACKUP_DIR` (`filepath.Rel` — an earlier `strings.TrimLeft` produced unusable paths).

### YouTube playlists (`pkg/bot/playlists.go`, `pkg/bot/yturl.go`)
- `playlists` is keyed by `(guild_id, youtube_playlist_id)`; title is metadata only, so renaming on YouTube never forks a playlist. `playlist_entries` stores membership + position only — no file paths, no download state.
- `/play <playlist url>` registers an unknown playlist with a full snapshot; a known one plays the **saved** snapshot. `/playlist name:<id> [update:true]` always plays the saved snapshot unless `update:true` explicitly re-syncs.
- `/playlist-delete name:<id>` is admin-only: it re-checks `PermissionAdministrator` at runtime (registration's `DefaultMemberPermissions` is only a UI hint), soft-deletes the row (`deleted_at`) and drops its entries in one transaction. The row is kept so re-discovery reactivates it instead of duplicating it. It never touches the backup DB or files.
- Snapshot writes go through `db.Store.WithTx` (upsert playlist → delete entries → insert positions → `last_synced_at`). No network I/O inside the transaction.
- One shared video cache: `cachedVideo` (`pkg/bot/videocache.go`) looks up the canonical `https://www.youtube.com/watch?v=ID` in the backup DB, verifies the file exists, and otherwise enqueues a `BackupJob`. Playlists do not own files; removing an entry never deletes an artefact.

### ffmpeg (`pkg/ffmpeg`)
`ToDiscordMP4`/`ToDiscordGIF` compute a target bitrate from `DiscordMaxFileSizeBytes` (20 MiB) and clip duration, re-encoding only when needed (oversized, muted, clipped, or HEVC — `HasHEVCVideo` via `ffprobe`). `FFMPEG_HW_ACCEL` env var toggles Intel QSV hardware acceleration (`h264_qsv`, seen as `-init_hw_device qsv=hw`).

### yt-dlp (`pkg/ytdlp`)
Wraps the `yt-dlp` binary (`DownloadAudio`, `DownloadVideo`, `DownloadVideoSmall`, `PlaylistInfo`). `StartUpdater` (called once from `main.go`) runs `yt-dlp -U` on an hourly ticker for the life of the process. Per-domain cookies when set: `YT_COOKIE_PATH` for youtube urls, `REDDIT_COOKIE_PATH` for reddit.com; other urls get no cookies. Reddit also gets `--impersonate firefox` — Reddit rejects raw TLS fingerprints ("Your IP address is unable to access the Reddit API"); needs the curl_cffi-bundled standalone build.

### minio — legacy only
`pkg/minio` and `cmd/migrage_from_minio` exist solely to migrate sound files *away* from a MinIO bucket into local backup storage (migration `008_remove_minio_dep.sql`). Not used by the running bot; the `MINIO_*` config vars are declared in `pkg/config` but never populated by `Load()` — only set by that one-off command before running.

### Other one-off `cmd/` binaries
- `cmd/importSounds` — one-time seed script, reads a hardcoded JSON blob of sound aliases/links into the store DB for one specific guild ID.
- `cmd/ffmpeg-debug` — standalone CLI (`-input`, `-mute`, `-start`, `-end`, `-output-dir` flags) to exercise `pkg/ffmpeg` conversions without running the bot.

## Config (`pkg/config`, loaded via `github.com/fr-str/env` in `Load()`)

| Var | Default | Notes |
|---|---|---|
| `ENV` | `dev` | `prod` triggers nothing special beyond being read elsewhere |
| `LOG_LEVEL` | `debug` | `debug`/`info`/`warn`/`error`, or `sql` to also show per-query SQL logs (see Logging above) |
| `TOKEN` | `""` | Discord bot token — **raw token**, no `"Bot "` prefix (disgo adds it itself; discordgo required the caller to prepend it) |
| `GUILD_ID` | `""` | single-guild bot, commands registered to this guild only |
| `DB_DIR` | `data` | main store DB directory, created if missing |
| `BACKUP_DIR` | `$TMPDIR/dbot/backup` | backup DB + attachment files, created if missing |
| `TMP_PATH` | `$TMPDIR/dbot` | audio cache DB, scratch files, per-command tmp dirs |
| `YT_COOKIE_PATH` | `""` | passed to yt-dlp `--cookies` for YouTube urls |
| `REDDIT_COOKIE_PATH` | `""` | passed to yt-dlp `--cookies` for reddit.com |
| `FFMPEG_HW_ACCEL` | `false` | enables QSV hardware transcode |

`docker-compose.yml` mounts `./.prod.env` as `/dbot/.env` and `./prod-data`/`./prod-tmp` as the real data dirs; `.env`/`.prod.env` are both gitignored.

## Code style (from existing `AGENTS.md`, still accurate)

- Imports in 3 groups (stdlib / external / `dbot/...`), blank line between.
- PascalCase exported, camelCase private; `Ctx` is the accepted abbreviation for `Context`.
- Return errors, don't panic — except `init()`/startup failures (DB connect, migrations, client construction all `panic` on error today).
- `context.Context` first param on cancellable functions.
- Tests: `github.com/stretchr/testify` (`require` to stop on failure, `assert` to continue), table-driven with `t.Run` subtests.
