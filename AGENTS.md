# AGENTS.md — dbot

Single-guild Discord bot (Go, disgo): music/sound playback, attachment backup with perceptual-hash dedupe, YouTube playlist snapshots, ffmpeg transcoding to fit Discord's upload limits.

## Build & run

- Local dev: `air` (hot reload; watches `.env`). Env vars are read from the process environment via `fr-str/env`; there is no dotenv loading in code — export them or use docker.
- CGO: `pkg/ffmpeg` links **libdave**. On Linux you must install it first (`make libdave`, Arch), then export `PKG_CONFIG_PATH` and `LD_LIBRARY_PATH` before any `CGO_ENABLED=1 go build`. Plain `go build ./...` (CGO off) fails to link — expected, not a bug.
- Docker: `make build` (mod tidy + image build; the Dockerfile installs libdave itself), `make compose` / `compose-build`. Compose mounts `.prod.env`, volumes for `prod-data`/`prod-tmp`, and exposes **:58008** (API server).

## Tests & CI

- `go test ./...`; single package: `go test -run 'TestName' ./pkg/bot`.
- Packages with tests: `bot`, `config`, `db/types`, `ffmpeg`, `job_runner`, `logger`, `player`, `ytdlp`. testify for assertions.
- CI (self-hosted runner): build → vet → `go test ./... -timeout 60s`. Keep the suite fast and hang-free.

## Database & codegen

- Two SQLite DBs, both WAL + `synchronous=NORMAL` set at connect in `pkg/db`, `MaxOpenConns(1)`:
  - **store** (`$DB_DIR`) — channel maps, sounds, playlists, queue state.
  - **backup** (`$BACKUP_DIR`) — backup artifacts + pHash dedupe records.
- sqlc generates both (two entries in `sqlc.yaml` → `pkg/store`, `pkg/backup`). Never hand-edit those packages; edit SQL in `db/migrations/`, `db/queries/*.sql`, or `db/backup/*.sql`, then `sqlc generate`.
- goose via the go.mod tool directive (`go tool goose ...`). Migrations auto-run at startup (embedded migration sets for both DBs). Up-only by convention — no down sections.
- Use `store.WithTx(ctx, fn)` for transactions; it must not do network work inside (single writer — a long txn blocks all other writes).

## Architecture notes (not obvious from filenames)

- disgo dispatches events synchronously by default. `main.go` requires `disgobot.WithAsyncEventsEnabled()`; handlers that wait on gateway events deadlock without it.
- Player: one loop goroutine (`soundLoop`) plus track playback via `playTrackFn`. Frame pacing uses a player-owned 20 ms clock. Do **not** hand the voice.Conn to disgo's `AudioSender`/`SetOpusFrameProvider`: music and sound share one Conn, and giving it away silently drops the other producer.
- Job queue: `pkg/job_runner` is a single serial polling loop (sleeps 5 s when idle). Only one job type is registered today: `BackupJob` → `bot.backupVideoJob`.
- ffmpeg outputs must fit Discord limits (~20 MiB video): `ToDiscordMP4`/`ToDiscordGIF` compute bitrate for that; `FFMPEG_HW_ACCEL` switches to QSV.
- yt-dlp wrapper in `pkg/ytdlp`; main.go runs an hourly `yt-dlp -U` updater; `YT_COOKIE_PATH`/`REDDIT_COOKIE_PATH` map to yt-dlp `--cookies` per URL domain; reddit requests also get `--impersonate firefox` (Reddit 403s raw TLS fingerprints). Dockerfile installs the `yt-dlp_linux` standalone build — the plain zipimport asset has no curl_cffi, so `--impersonate` fails.
- API server on :58008 (`pkg/api`): `/api/dst/links`, `/api/init-scrape`.

## Logging & config

- stdlib slog through the custom handler in `pkg/logger` (fr-str/log is gone — don't import it). `LOG_LEVEL`: default `debug`; use `sql` to log every query's SQL.
- Error style: wrap with `%w`, prefix user-facing bot errors with `"dbot:"` (`bot.dbotErr`).

| Env var | Default | Purpose |
|---|---|---|
| `TOKEN`, `GUILD_ID` | — (required) | Discord auth / guild |
| `DB_DIR` | `$TMPDIR/dbot` | store DB dir |
| `BACKUP_DIR` | `$TMPDIR/dbot/backup` | backup DB + artifacts |
| `ENV` | — | dev/prod marker |
| `LOG_LEVEL` | `debug` (`sql` for SQL) | log level |
| `YT_COOKIE_PATH` | — | yt-dlp cookies for YouTube urls |
| `REDDIT_COOKIE_PATH` | — | yt-dlp cookies for reddit.com |
| `FFMPEG_HW_ACCEL` | off (bool) | enable QSV |

## One-shot tools under cmd/

- `cmd/importSounds` — seed script; `cmd/ffmpeg-debug` — standalone ffmpeg CLI against `pkg/ffmpeg`; `cmd/requeue-missing` — requeue missing cached videos (`-n` dry run).
- `cmd/backfill-titles` and `cmd/img` are empty leftover dirs — no code there.
