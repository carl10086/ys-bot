-- sqlc 查询定义（生成的 Go 代码在 ./gen，不手改）。
-- 关键正确性逻辑都在这里：msg_id 去重、父行计数器发号。

-- name: CreateSession :one
INSERT INTO sessions (id, user_id, title)
VALUES ($1, $2, $3)
RETURNING id, user_id, title, created_at;

-- name: GetSession :one
SELECT id, user_id, title, created_at FROM sessions WHERE id = $1;

-- name: NextSeq :one
-- 父行计数器发号：行锁串行化同会话插入，事务回滚不留空号。
UPDATE sessions SET last_seq = last_seq + 1 WHERE id = $1
RETURNING last_seq;

-- name: InsertMessage :one
-- user_id 从 sessions 父行派生（单一事实源，不靠应用传）。
-- msg_id 冲突（客户端重发/并发竞态）→ DO NOTHING 不返行，调用方识别为重复。
INSERT INTO messages (id, session_id, seq, user_id, kind, content, msg_id, turn_id)
VALUES ($1, $2, $3, (SELECT user_id FROM sessions WHERE id = $2), $4, $5, $6, $7)
ON CONFLICT (msg_id) DO NOTHING
RETURNING seq;

-- name: GetMessageSeqByMsgID :one
SELECT seq FROM messages WHERE msg_id = $1;

-- name: ListMessagesAfter :many
SELECT session_id, seq, kind, content, msg_id, turn_id, created_at
FROM messages
WHERE session_id = $1 AND seq > $2
ORDER BY seq;
