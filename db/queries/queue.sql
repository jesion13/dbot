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
