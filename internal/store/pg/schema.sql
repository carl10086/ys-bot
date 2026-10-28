-- M1 业务 schema（docs/architecture/data-model.md 统一建表约定）。
-- sandboxes 表 M2 再建（YAGNI）。

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
