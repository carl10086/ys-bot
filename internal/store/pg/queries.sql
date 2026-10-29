-- sqlc 查询定义（生成的 Go 代码在 ./gen，不手改）。
-- 关键正确性逻辑都在这里：msg_id 去重、父行计数器发号。

-- name: CreateSession :one
INSERT INTO sessions (id, user_id, title)
VALUES ($1, $2, $3)
RETURNING id, user_id, title, created_at;

-- name: GetSession :one
SELECT id, user_id, title, created_at FROM sessions WHERE id = $1;

-- name: ListSessionsByUser :many
-- SPA 会话列表：最近活跃倒序（有消息按最后一条消息时间，无消息按创建时间）。
SELECT s.id, s.user_id, s.title, s.created_at,
       COALESCE(MAX(m.created_at), s.created_at) AS last_active_at
FROM sessions s
LEFT JOIN messages m ON m.session_id = s.id
WHERE s.user_id = $1
GROUP BY s.id
ORDER BY last_active_at DESC;

-- name: UpdateSessionTitle :one
UPDATE sessions SET title = $2 WHERE id = $1
RETURNING id, user_id, title, created_at;

-- name: DeleteSessionMessages :exec
DELETE FROM messages WHERE session_id = $1;

-- name: DeleteSession :exec
DELETE FROM sessions WHERE id = $1;

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

-- ===== sandboxes（M2；vmhostd 管 slot/status，worker/server 管建行）=====

-- name: CreateSandbox :one
INSERT INTO sandboxes (id, user_id, host_id, status)
VALUES ($1, $2, $3, 'creating')
RETURNING id, user_id, host_id, status, slot, vm_ip, created_at;

-- name: GetSandboxByID :one
SELECT id, user_id, host_id, status, slot, vm_ip, created_at FROM sandboxes WHERE id = $1;

-- name: GetSandboxByUser :one
SELECT id, user_id, host_id, status, slot, vm_ip, created_at FROM sandboxes WHERE user_id = $1;

-- name: ListUsedSlotsByHost :many
SELECT slot FROM sandboxes WHERE host_id = $1 AND slot IS NOT NULL;

-- name: ListActiveSandboxesByHost :many
-- vmhostd 启动 reconcile 用：本宿主上 PG 认为还活着的 VM。
SELECT id, user_id, host_id, status, slot, vm_ip, created_at
FROM sandboxes
WHERE host_id = $1 AND status IN ('creating', 'awake');

-- name: SetSandboxAwake :one
-- slot 分配与置 awake 同事务；UNIQUE(host_id,slot) 兜底并发冲突。
UPDATE sandboxes SET slot = $2, vm_ip = $3, status = 'awake'
WHERE id = $1
RETURNING id;

-- name: SetSandboxStatus :exec
UPDATE sandboxes SET status = $2 WHERE id = $1;

-- name: ReleaseSandbox :exec
-- DestroyVM / reconcile：释放 slot 与地址，行保留（历史可查）。
UPDATE sandboxes SET status = 'stopped', slot = NULL, vm_ip = NULL
WHERE id = $1;
