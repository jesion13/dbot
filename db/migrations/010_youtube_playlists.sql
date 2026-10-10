-- +goose up
-- Playlists are now identified by their stable YouTube playlist id (not by title).
--
-- Legacy rows are deliberately discarded rather than mapped onto invented ids:
-- the old schema had no youtube playlist id, only a user supplied name plus a
-- playlist URL that may or may not carry a list= parameter.
--
-- NOTE: no goose down section - sqlc replays down statements as if they were up,
-- which would resurrect the old schema for every future generation.
DROP TABLE IF EXISTS playlist_entries;
DROP TABLE IF EXISTS playlists;

CREATE TABLE playlists (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    guild_id TEXT NOT NULL,
    youtube_playlist_id TEXT NOT NULL,
    title TEXT NOT NULL,
    youtube_url TEXT NOT NULL,
    created_at DATETIME DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    last_synced_at DATETIME DEFAULT NULL,
    deleted_at DATETIME DEFAULT NULL
);

-- playlist identity: same guild + same youtube playlist id == same playlist,
-- no matter how often the title changes.
CREATE UNIQUE INDEX IF NOT EXISTS idx_playlists_guild_youtube_id
ON playlists (guild_id, youtube_playlist_id);

CREATE TABLE playlist_entries (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    playlist_id INTEGER NOT NULL,
    youtube_video_id TEXT,
    youtube_url TEXT NOT NULL,
    title TEXT NOT NULL,
    position INTEGER NOT NULL,
    FOREIGN KEY (playlist_id) REFERENCES playlists(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_playlist_entries_playlist_id ON playlist_entries(playlist_id);
-- a snapshot is rewritten by delete-all + insert, so positions stay unique per playlist.
CREATE UNIQUE INDEX IF NOT EXISTS idx_playlist_entries_position
ON playlist_entries (playlist_id, position);