-- M1 业务 schema（docs/architecture/data-model.md 定稿）。
-- sandboxes 表 M2 再建（YAGNI）。

CREATE TABLE IF NOT EXISTS sessions (
  id         uuid PRIMARY KEY,              -- uuidv7，server 生成
  user_id    text NOT NULL,                 -- v1 常量 'local'；无 FK（定稿决策）
  title      text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS messages (
  session_id uuid NOT NULL REFERENCES sessions(id),
  seq        bigint NOT NULL,               -- 会话内递增（讲座 15 Ordering）
  kind       text NOT NULL,                 -- user / assistant / tool_call / tool_result
  content    jsonb NOT NULL,
  msg_id     uuid UNIQUE,                   -- 仅用户消息有；客户端生成，幂等兜底
  turn_id    text,                          -- = Temporal workflowID
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (session_id, seq)
);
