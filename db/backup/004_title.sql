-- +goose Up
-- The title used to live only inside the stored filename, which the code parsed back
-- out on every cache hit. That worked for YouTube but not for direct links, where
-- yt-dlp has no real video id, and it broke outright once --trim-filenames truncated a
-- long name. Storing it makes the filename free to be short and unique.
ALTER TABLE artefacts ADD COLUMN title TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE artefacts DROP COLUMN title;
