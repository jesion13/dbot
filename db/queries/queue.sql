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

-- name: NextInQueue :one
SELECT * FROM queue
WHERE status != 'done' and fail_count < 5 order by id asc;

-- name: FindFailedTasksInQueue :many
SELECT * FROM queue
WHERE status != 'done' and fail_count = 5 order by id asc;

-- name: CountQueueByJobType :one
SELECT count(*) FROM queue WHERE job_type = :job_type;

-- queue rows are never deleted, so an existing row means this exact job already ran:
-- either the artefact exists (cachedVideo would have found it) or it failed for good.
-- name: QueueHasJob :one
SELECT count(*) FROM queue WHERE job_type = :job_type AND meta = :meta;
