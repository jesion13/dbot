-- +goose up

-- The dupe scan compares one incoming hash against every recent artefact in the guild.
-- Banding cuts that down: split each 64-bit hash into 4 chunks of 16 bits, stored as
-- generated columns so they can be indexed. Pigeonhole: two hashes within
-- dupeThreshold (3) bits must agree exactly on at least one chunk, because 3 flipped
-- bits cannot touch all 4 chunks. So a per-chunk equality test keeps every real match
-- and drops the rest inside SQLite instead of shipping the whole window to Go.
--
-- Four columns rather than one: a single `band IN (chunk0, chunk1, ...)` would compare
-- every stored chunk against every query chunk, which is not the pigeonhole test at all
-- and silently misses real duplicates. The chunks must be compared position by position,
-- hence GetArtefactCandidates as a UNION of one branch per chunk.
--
-- Index excludes media_type on purpose: it would turn each branch into four seeks, and
-- every non-image artefact has hash 0 so the chunk test alone already skips them. The
-- caller re-checks the real Hamming distance, so a stray candidate costs nothing.
--
-- The guarantee needs chunkCount > dupeThreshold, so raising the threshold above 3
-- without adding chunks starts losing matches silently. TestDupeBandingCoversThreshold
-- pins that.
-- one statement per column: SQLite allows a combined ALTER but sqlc's schema parser
-- does not, and it reads this directory to type the generated code.
ALTER TABLE artefacts ADD COLUMN band0 INTEGER GENERATED ALWAYS AS ((hash >> 48) & 0xFFFF) VIRTUAL;
ALTER TABLE artefacts ADD COLUMN band1 INTEGER GENERATED ALWAYS AS ((hash >> 32) & 0xFFFF) VIRTUAL;
ALTER TABLE artefacts ADD COLUMN band2 INTEGER GENERATED ALWAYS AS ((hash >> 16) & 0xFFFF) VIRTUAL;
ALTER TABLE artefacts ADD COLUMN band3 INTEGER GENERATED ALWAYS AS ( hash        & 0xFFFF) VIRTUAL;

CREATE INDEX IF NOT EXISTS idx_artefacts_gid_band0 ON artefacts(gid, band0);
CREATE INDEX IF NOT EXISTS idx_artefacts_gid_band1 ON artefacts(gid, band1);
CREATE INDEX IF NOT EXISTS idx_artefacts_gid_band2 ON artefacts(gid, band2);
CREATE INDEX IF NOT EXISTS idx_artefacts_gid_band3 ON artefacts(gid, band3);