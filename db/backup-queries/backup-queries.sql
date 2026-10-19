
-- name: InsertBackup :exec
INSERT INTO msg_backup (msg_id, channel_id,author_id, content, attachments, created_at)
VALUES (:msg_id, :channel_id,:author_id,  :content, :attachments, :created_at);

-- name: UpdateBackupMsg :exec
UPDATE msg_backup SET content = :content
WHERE msg_id = :msg_id;


-- name: InsertArtefact :exec
INSERT INTO artefacts (origin_url,path, media_type, hash, created_at,gid,chid,msgid,title)
VALUES (:origin_url,:path, :media_type, :hash, :created_at, :gid, :chid, :msgid, :title);

-- repairs an existing artefact instead of failing on the origin_url primary key
-- name: RepairArtefact :exec
UPDATE artefacts
SET path = :path,
    media_type = :media_type,
    hash = :hash,
    gid = :gid,
    chid = :chid,
    msgid = :msgid,
    title = :title
WHERE origin_url = :origin_url;

-- The dupe scan runs on every posted image and compares against the whole archive, so
-- it only looks at a recent window. Older uploads are never reported as duplicates.
--
-- One branch per hash chunk, each hitting its own index. UNION ALL (not a single
-- `band IN (...)`): the pigeonhole test needs chunk i compared with chunk i, and SQLite
-- only picks the per-branch indexes for a UNION or, after ANALYZE, for a MULTI-INDEX OR.
-- A row matching on two chunks comes back twice; that is harmless because the caller
-- re-checks the real Hamming distance.
-- name: GetArtefactCandidates :many
SELECT * FROM artefacts
WHERE artefacts.gid = :gid
  AND artefacts.media_type in ('image/jpg','image/jpeg','image/png','image/webp')
  AND artefacts.created_at >= :created_at AND artefacts.band0 = :band0
UNION ALL
SELECT * FROM artefacts
WHERE artefacts.gid = :gid
  AND artefacts.media_type in ('image/jpg','image/jpeg','image/png','image/webp')
  AND artefacts.created_at >= :created_at AND artefacts.band1 = :band1
UNION ALL
SELECT * FROM artefacts
WHERE artefacts.gid = :gid
  AND artefacts.media_type in ('image/jpg','image/jpeg','image/png','image/webp')
  AND artefacts.created_at >= :created_at AND artefacts.band2 = :band2
UNION ALL
SELECT * FROM artefacts
WHERE artefacts.gid = :gid
  AND artefacts.media_type in ('image/jpg','image/jpeg','image/png','image/webp')
  AND artefacts.created_at >= :created_at AND artefacts.band3 = :band3;

-- name: GetArtefactsByURLs :many
SELECT * FROM artefacts
WHERE origin_url IN (sqlc.slice('origin_urls'));

-- name: DeleteArtefact :exec
DELETE FROM artefacts WHERE gid = :gid AND chid = :chid and msgid = :msgid;

-- The backfill reads titles out of the filenames the old scheme produced, so it only
-- has to look at the video directory.
-- name: ListVideoArtefacts :many
SELECT * FROM artefacts WHERE path LIKE '%/play_history/%';

-- name: SetArtefactTitle :exec
UPDATE artefacts SET title = :title WHERE origin_url = :origin_url;

-- name: GetArtefact :one
SELECT * FROM artefacts WHERE origin_url = :origin_url;

-- The unfiltered counterparty to GetArtefactCandidates: no banding, no window. It exists
-- so the dupe tests can compare the prefiltered scan against a full scan of the guild.
-- name: AllArtefacts :many
SELECT * FROM artefacts WHERE gid = :gid;


-- name: UpsertUser :one
INSERT INTO users (discord_id, username)
VALUES (:discord_id, :username)
ON CONFLICT (discord_id) DO UPDATE SET
    username = excluded.username
RETURNING *;
