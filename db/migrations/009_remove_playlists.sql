-- +goose up
DROP TABLE IF EXISTS playlist_entries;
DROP TABLE IF EXISTS playlists;

-- pending async download jobs were only enqueued by save-playlist
DELETE FROM queue WHERE job_type = 'download';