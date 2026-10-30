-- M1 业务 schema（docs/architecture/data-model.md 统一建表约定）。
-- sandboxes 表 M2 建（docs/ys-powers/specs/2026-10-29-m2-design.md）。

CREATE TABLE IF NOT EXISTS sandboxes (
  id               uuid PRIMARY KEY,              -- uuidv7，应用生成
  user_id          text NOT NULL,                 -- 一用户一台（讲座 09）；UNIQUE
  host_id          text NOT NULL,                 -- VM 所在宿主（slot 语义前提）
  status           text NOT NULL,                 -- creating / awake / sleeping / stopped / failed
  slot             int,                           -- 宿主本地 IP 编号；NULL = 未分配
  vm_ip            text,                          -- 由 slot 推出的 agentd 地址（含端口）
  snapshot_ref     text,                          -- M4：快照引用（休眠成功时写入，销毁/降级 boot 清空）
  last_activity_at timestamptz,                   -- M4：最近活动时间（sweeper 过期判据）
  created_at       timestamptz NOT NULL DEFAULT now(),
  UNIQUE (host_id, slot),                         -- slot 宿主内唯一（NULL 不冲突）
  UNIQUE (user_id)
);

-- M4 sweeper 扫描：按状态+最近活动找过期 awake sandbox。
CREATE INDEX IF NOT EXISTS sandboxes_awake_idle ON sandboxes (status, last_activity_at);

CREATE TABLE IF NOT EXISTS sessions (
  id         uuid PRIMARY KEY,              -- uuidv7，应用生成
  user_id    text NOT NULL,                 -- 租户维度；无 FK（定稿决策）
  title      text NOT NULL DEFAULT '',      -- 易读名称，SPA 可设
  last_seq   bigint NOT NULL DEFAULT 0,     -- 父行计数器：会话内 seq 发号器
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS messages (
  id         uuid PRIMARY KEY,              -- uuidv7，应用生成（统一约定：单列代理 PK）
  session_id uuid NOT NULL REFERENCES sessions(id),
  seq        bigint NOT NULL,               -- 会话内递增（父行计数器发放）
  user_id    text NOT NULL,                 -- 冗余自 sessions.user_id（RLS/按用户直查）
  kind       text NOT NULL,                 -- user / assistant / tool_call / tool_result
  content    jsonb NOT NULL,
  msg_id     uuid UNIQUE,                   -- 仅用户消息有；客户端生成，幂等兜底
  turn_id    text,                          -- = Temporal workflowID；仅 assistant 消息有
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (session_id, seq)                  -- 业务唯一性；PK 是 id
);
