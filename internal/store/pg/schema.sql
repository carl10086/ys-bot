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

-- ===== M5 记忆与技能（spec: 2026-11-02-m5-memory-design.md；选型 ADR-0002）=====
-- 记忆是信息不是指令（讲座 30）：事实进 memories，流程进 skills。

CREATE TABLE IF NOT EXISTS memories (
  id          uuid PRIMARY KEY,              -- uuidv7，应用生成
  user_id     text NOT NULL,                 -- 记忆归 user 不归 session（跨会话共享）
  type        text NOT NULL,                 -- profile / log / note
  text        text NOT NULL,                 -- 一条事实（markdown）
  importance  int  NOT NULL DEFAULT 5,       -- 提取时 LLM 打分 1-10，注入排序用
  hash        text NOT NULL,                 -- md5(text)：同文本不重复入库
  expires_at  timestamptz,                   -- 仅 note：消退时间；NULL = 不消退
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (user_id, hash)
);

-- 注入扫描：按用户+类型取（profile 全量 / log 最近 30 / 活跃 note）。
CREATE INDEX IF NOT EXISTS memories_user_type ON memories (user_id, type);

CREATE TABLE IF NOT EXISTS skills (
  id          uuid PRIMARY KEY,
  user_id     text NOT NULL,
  name        text NOT NULL,                 -- 注入清单用
  description text NOT NULL DEFAULT '',      -- 一句话，agent 据此决定读不读全文
  content     text NOT NULL,                 -- markdown 全文，memory_read 读
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (user_id, name)
);

-- 审计流水：每次记忆写操作留痕（"它为什么记住了这个"必须可追溯）。
-- 保留策略待定：按 M5 体量（每天几十轮 × 几 ops）一年几十万行，现在不是事；
-- 数据量起来后按 created_at 分区或定期归档（同 M4 快照"只留最新"的生命周期意识）。
CREATE TABLE IF NOT EXISTS memory_ops (
  id          bigserial PRIMARY KEY,
  user_id     text NOT NULL,
  op          text NOT NULL,                 -- add / update / delete
  target_type text NOT NULL,                 -- memory / skill
  target_id   uuid,
  before_text text,
  after_text  text,
  source      text NOT NULL,                 -- pass:<turn_id> / agent
  created_at  timestamptz NOT NULL DEFAULT now()
);

-- memory pass 快照版本按用户读 max(id)，避免随审计量增长退化成全表扫描。
CREATE INDEX IF NOT EXISTS memory_ops_user_id_id ON memory_ops (user_id, id DESC);
