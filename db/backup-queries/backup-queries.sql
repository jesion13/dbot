
-- name: InsertBackup :exec
INSERT INTO msg_backup (msg_id, channel_id,author_id, content, attachments, created_at)
VALUES (:msg_id, :channel_id,:author_id,  :content, :attachments, :created_at);

-- name: UpdateBackupMsg :exec
UPDATE msg_backup SET content = :content
WHERE msg_id = :msg_id;


-- name: InsertArtefact :exec
INSERT INTO artefacts (origin_url,path, media_type, hash, created_at,gid,chid,msgid)
VALUES (:origin_url,:path, :media_type, :hash, :created_at, :gid, :chid, :msgid);

-- repairs an existing artefact instead of failing on the origin_url primary key
-- name: RepairArtefact :exec
UPDATE artefacts
SET path = :path,
    media_type = :media_type,
    hash = :hash,
    gid = :gid,
    chid = :chid,
    msgid = :msgid
WHERE origin_url = :origin_url;

-- The dupe scan runs on every posted image and compares against the whole archive, so
-- it only looks at a recent window. Older uploads are never reported as duplicates.
-- name: GetArtefacts :many
SELECT * FROM artefacts
WHERE
    media_type in ('image/jpg','image/jpeg','image/png','image/webp')
AND
    gid = ?
AND
    created_at >= ?;

-- name: GetArtefactsByURLs :many
SELECT * FROM artefacts
WHERE origin_url IN (sqlc.slice('origin_urls'));

-- name: DeleteArtefact :exec
DELETE FROM artefacts WHERE gid = :gid AND chid = :chid and msgid = :msgid;

-- name: GetArtefact :one
SELECT * FROM artefacts WHERE origin_url = :origin_url;


-- name: UpsertUser :one
INSERT INTO users (discord_id, username)
VALUES (:discord_id, :username)
ON CONFLICT (discord_id) DO UPDATE SET
    username = excluded.username
RETURNING *;
