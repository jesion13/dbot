-- name: GetChannel :one
SELECT * FROM channels
WHERE gid = ? AND type = ? LIMIT 1;

-- upsert
-- name: MapChannel :one
INSERT INTO channels (gid,chid, ch_name, type)
VALUES (:gid, :chid, :ch_name, :type)
ON CONFLICT DO UPDATE SET
    chid = excluded.chid,
    ch_name = excluded.ch_name
RETURNING *;

-- name: DeleteChannel :exec
DELETE FROM channels
WHERE chid = ?;


-- name: AddSound :one
INSERT INTO sounds (gid,url,origin_url,aliases)
VALUES (:gid,:url,:origin_url,:aliases)
RETURNING *;

-- name: SelectSounds :many 
SELECT * FROM sounds
WHERE gid = ?;


-- name: GetPlayerPanel :one
SELECT * FROM player_panels
WHERE guild_id = ? LIMIT 1;

-- name: UpsertPlayerPanel :one
INSERT INTO player_panels (guild_id, channel_id, message_id, generation)
VALUES (:guild_id, :channel_id, :message_id, :generation)
ON CONFLICT (guild_id) DO UPDATE SET
    channel_id = excluded.channel_id,
    message_id = excluded.message_id,
    generation = excluded.generation,
    updated_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
RETURNING *;

-- name: DeletePlayerPanel :exec
DELETE FROM player_panels
WHERE guild_id = ?;
