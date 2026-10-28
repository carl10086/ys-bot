-- sqlc 查询定义（生成的 Go 代码在 ./gen，不手改）。
-- 关键正确性逻辑都在这里：msg_id 去重、会话内 seq 递增。

-- name: CreateSession :one
INSERT INTO sessions (id, user_id, title)
VALUES ($1, $2, $3)
RETURNING id, user_id, title, created_at;

-- name: LockSessionForSeq :exec
-- 会话级 advisory xact lock：防并发插入时 seq 撞 PK。
SELECT pg_advisory_xact_lock(hashtext($1::text));

-- name: InsertMessageDedup :one
-- msg_id 冲突（客户端重发）→ DO NOTHING 不返行，调用方识别为重复。
INSERT INTO messages (session_id, seq, kind, content, msg_id, turn_id)
VALUES ($1, (SELECT COALESCE(MAX(seq), 0) + 1 FROM messages WHERE session_id = $1), $2, $3, $4, $5)
ON CONFLICT (msg_id) DO NOTHING
RETURNING seq;

-- name: GetMessageSeqByMsgID :one
SELECT seq FROM messages WHERE msg_id = $1;

-- name: ListMessagesAfter :many
SELECT session_id, seq, kind, content, msg_id, turn_id, created_at
FROM messages
WHERE session_id = $1 AND seq > $2
ORDER BY seq;
