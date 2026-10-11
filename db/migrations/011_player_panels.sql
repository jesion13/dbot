-- Capitalised: goose accepts either, but sqlc only recognises Up/Down and applies the
-- whole file when it does not, which dropped this table again before it could be read.
-- +goose Up
-- The panel is a single message the bot owns, but which one that is has to survive a
-- restart: without this, every deploy leaves an orphaned panel behind that nothing
-- will ever delete. One row per guild.
CREATE TABLE IF NOT EXISTS player_panels (
    guild_id TEXT NOT NULL PRIMARY KEY,
    channel_id TEXT NOT NULL,
    message_id TEXT NOT NULL,
    generation INTEGER NOT NULL DEFAULT 1,
    updated_at DATETIME DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

-- +goose Down
DROP TABLE IF EXISTS player_panels;