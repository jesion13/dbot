-- name: Enqueue :one
INSERT INTO queue (meta,fail_count,last_msg,status,job_type)
VALUES (
    :meta,
    :fail_count,
    :last_msg,
    :status,
    :job_type
) RETURNING *;

-- name: UpdateQueueEntry :exec
UPDATE queue SET
fail_count = :fail_count,
last_msg = :last_msg,
status = :status
WHERE id = :id;

-- 'unavailable' is a terminal status too: without it a permanently failed job stays
-- eligible (fail_count never moved) and the runner picks it up again forever.
-- name: NextInQueue :one
SELECT * FROM queue
WHERE status NOT IN ('done', 'unavailable') and fail_count < 3 order by id asc;

-- >= so that rows written under the old 5-attempt policy (fail_count 4 or 5) stay
-- visible as exhausted instead of vanishing
-- name: FindFailedTasksInQueue :many
SELECT * FROM queue
WHERE status NOT IN ('done', 'unavailable') and fail_count >= 3 order by id asc;

-- name: CountQueueByJobType :one
SELECT count(*) FROM queue WHERE job_type = :job_type;

-- The *latest* job for this exact metadata decides the current state of that video.
-- Older terminal rows must not outvote a newer record.
-- name: LatestJob :one
SELECT * FROM queue
WHERE job_type = :job_type AND meta = :meta
ORDER BY id DESC
LIMIT 1;