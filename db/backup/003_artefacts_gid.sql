-- +goose up
-- checkHashAgainstDB() reads every image row for a guild on each message with an
-- attachment, filtered on gid and media_type. Without this index that is a full
-- table scan of the archive on every posted picture.
CREATE INDEX IF NOT EXISTS idx_artefacts_gid_media_type ON artefacts(gid, media_type);