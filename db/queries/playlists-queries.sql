-- name: UpsertPlaylist :one
INSERT INTO playlists (guild_id, youtube_playlist_id, title, youtube_url)
VALUES (:guild_id, :youtube_playlist_id, :title, :youtube_url)
ON CONFLICT (guild_id, youtube_playlist_id) DO UPDATE SET
    title = excluded.title,
    youtube_url = excluded.youtube_url,
    deleted_at = NULL
RETURNING *;

-- name: GetPlaylistByYoutubeID :one
SELECT * FROM playlists
WHERE guild_id = :guild_id
  AND youtube_playlist_id = :youtube_playlist_id
  AND deleted_at IS NULL
LIMIT 1;

-- name: GetPlaylist :one
SELECT * FROM playlists
WHERE id = :id
  AND guild_id = :guild_id
  AND deleted_at IS NULL
LIMIT 1;

-- name: SearchPlaylists :many
SELECT * FROM playlists
WHERE guild_id = :guild_id
  AND deleted_at IS NULL
  AND youtube_playlist_id != ''
  AND (:title = '' OR lower(title) LIKE '%' || lower(:title) || '%')
ORDER BY title COLLATE NOCASE, id
LIMIT 25;

-- name: DeletePlaylistEntries :exec
DELETE FROM playlist_entries
WHERE playlist_id = :playlist_id;

-- name: AddPlaylistEntry :exec
INSERT INTO playlist_entries (playlist_id, youtube_video_id, youtube_url, title, position)
VALUES (:playlist_id, :youtube_video_id, :youtube_url, :title, :position);

-- name: ListPlaylistEntries :many
SELECT * FROM playlist_entries
WHERE playlist_id = :playlist_id
ORDER BY position ASC;

-- name: TouchPlaylistSync :exec
UPDATE playlists
SET last_synced_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
WHERE id = :id;