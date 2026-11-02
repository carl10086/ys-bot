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
-- DestroyVM / reconcile：释放 slot 与地址，行保留（历史可查）。M4：快照引用一并清。
UPDATE sandboxes SET status = 'stopped', slot = NULL, vm_ip = NULL, snapshot_ref = NULL
WHERE id = $1;

-- ===== M4 快照休眠（spec: 2026-10-30-m4-snapshot-sleep-design.md）=====

-- name: RefreshSandboxActivity :exec
-- 续期写入点（workflow.ExecTool）：有任何活动就刷成 now()。
UPDATE sandboxes SET last_activity_at = now() WHERE id = $1;

-- name: RefreshSandboxActivityBySession :exec
-- 续期写入点（hitsend.Submit）：session → user → sandbox 一跳完成。
-- 无 sandbox 行（从未用过 VM）= 0 行更新，无害。
UPDATE sandboxes SET last_activity_at = now()
WHERE user_id = (SELECT user_id FROM sessions WHERE sessions.id = $1);

-- name: ListSleepCandidates :many
-- sweeper 全局扫描：过期（$1 分钟无活动）的 awake sandbox。跨宿主——路由信息在 host_id 里。
SELECT id, user_id, host_id, status, slot, vm_ip, created_at
FROM sandboxes
WHERE status = 'awake' AND last_activity_at < now() - $1::int * interval '1 minute';

-- name: TryStartSleep :execrows
-- 休眠入场券（never-mid-turn 的互斥核心）：检查与占坑同一原子操作。
-- 0 行 = 刚有活动/别人在睡/非 awake → 放弃；1 行 = 抢到，可以动手休眠。
UPDATE sandboxes SET status = 'sleeping'
WHERE id = $1 AND status = 'awake'
  AND last_activity_at < now() - $2::int * interval '1 minute';

-- name: SetSandboxSnapshotRef :exec
-- 休眠成功写快照路径；降级 boot / 销毁后清 NULL（NULL = 无快照可恢复）。
UPDATE sandboxes SET snapshot_ref = $2 WHERE id = $1;

-- name: ListSleepingSandboxesByHost :many
-- vmhostd reconcile 用：本宿主 sleeping 的 VM（快照丢失检测——丢了就转 stopped）。
SELECT id, user_id, host_id, status, slot, vm_ip, created_at
FROM sandboxes
WHERE host_id = $1 AND status = 'sleeping';

-- ===== M5 记忆与技能（spec: 2026-11-02-m5-memory-design.md）=====

-- name: AddMemory :execrows
-- hash 撞车（同文本已在库）= 去重，0 行返回。
INSERT INTO memories (id, user_id, type, text, importance, hash, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (user_id, hash) DO NOTHING;

-- name: UpdateMemory :execrows
UPDATE memories SET text = $2, importance = $3, expires_at = $4, updated_at = now()
WHERE id = $1;

-- name: DeleteMemory :execrows
DELETE FROM memories WHERE id = $1;

-- name: GetMemoryByID :one
SELECT id, user_id, type, text, importance, hash, expires_at, created_at, updated_at
FROM memories WHERE id = $1;

-- name: ListProfileMemories :many
-- 注入：核心事实全量（上限 100 条，重要性优先）。
SELECT id, user_id, type, text, importance, hash, expires_at, created_at, updated_at
FROM memories
WHERE user_id = $1 AND type = 'profile'
ORDER BY importance DESC, updated_at DESC
LIMIT 100;

-- name: ListRecentLogMemories :many
-- 注入：事件日志最近 30 条（讲座 30 "30 most relevant recent"）。
-- id 做次序兜底：同事务批量插入的 created_at 相同（now() = 事务起点），uuidv7 单调。
SELECT id, user_id, type, text, importance, hash, expires_at, created_at, updated_at
FROM memories
WHERE user_id = $1 AND type = 'log'
ORDER BY created_at DESC, id DESC
LIMIT 30;

-- name: ListActiveNotes :many
-- 注入：未消退的便签（expires_at NULL = 不消退）。
SELECT id, user_id, type, text, importance, hash, expires_at, created_at, updated_at
FROM memories
WHERE user_id = $1 AND type = 'note'
  AND (expires_at IS NULL OR expires_at > now())
ORDER BY created_at DESC;

-- name: ListAllMemoriesForUser :many
-- 提取上下文：全部记忆（过期 note 已消退，不给提取模型看——"只能删见过的"）。
SELECT id, user_id, type, text, importance, hash, expires_at, created_at, updated_at
FROM memories
WHERE user_id = $1
  AND (type != 'note' OR expires_at IS NULL OR expires_at > now())
ORDER BY created_at;

-- name: UpsertSkill :one
-- 同名覆盖写：技能按 (user_id, name) 唯一，add/update 都走这里。
INSERT INTO skills (id, user_id, name, description, content)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (user_id, name) DO UPDATE
SET description = $4, content = $5, updated_at = now()
RETURNING id;

-- name: DeleteSkill :execrows
DELETE FROM skills WHERE user_id = $1 AND name = $2;

-- name: GetSkillByName :one
SELECT id, user_id, name, description, content, created_at, updated_at
FROM skills WHERE user_id = $1 AND name = $2;

-- name: GetSkillByID :one
-- memory_read 工具用（search 命中的 skill 按 id 读全文）。
SELECT id, user_id, name, description, content, created_at, updated_at
FROM skills WHERE id = $1;

-- name: ListSkillCatalog :many
-- 注入清单：只有 name+description；全文靠 memory_read 按需读。
SELECT name, description FROM skills WHERE user_id = $1 ORDER BY name;

-- name: ListSkillsForUser :many
-- 提取上下文：id + 清单字段（提取模型要能对 skill 发 update/delete）。
SELECT id, name, description FROM skills WHERE user_id = $1 ORDER BY name;

-- name: SearchMemories :many
-- memory_search 工具（v1 关键词）：记忆正文 + 技能全文/名称。向量检索留作演进（ADR-0002）。
SELECT id, kind, text FROM (
  SELECT id, 'memory' AS kind, text, updated_at FROM memories
  WHERE memories.user_id = $1 AND text ILIKE '%' || $2 || '%'
  UNION ALL
  SELECT id, 'skill' AS kind, content AS text, updated_at FROM skills
  WHERE skills.user_id = $1 AND (content ILIKE '%' || $2 || '%' OR name ILIKE '%' || $2 || '%' OR description ILIKE '%' || $2 || '%')
) hits
ORDER BY updated_at DESC
LIMIT $3;

-- name: InsertMemoryOp :exec
INSERT INTO memory_ops (user_id, op, target_type, target_id, before_text, after_text, source)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: ListTurnMessages :many
-- pass 的提取原料 = 本轮消息。turn_id 仅 assistant 行有（schema 注释），
-- 用户消息靠 turn 的 batch seq 列表兜。
SELECT session_id, seq, kind, content, msg_id, turn_id, created_at
FROM messages
WHERE session_id = $1 AND (turn_id = $2 OR seq = ANY($3::bigint[]))
ORDER BY seq;
