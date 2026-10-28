# 数据模型（v1 定稿）

> 状态：**2026-10-28 经用户确认定稿**；同日二次确认修订：新增"统一建表约定"（user_id 全表冗余、单列代理 PK、父行计数器发号）。
> 依据：CS146S 讲座全档 `docs/reference/cs146s-grokbot-lecture.html`（引用一律用讲座序 01–37；sNN 文件号仅溯源）。
> 总原则：**事实源只写 PG 和文件存储；Redis 只放可丢的协调态；Temporal PG 是执行账本，应用不查。**

## 统一建表约定（2026-10-28 二次确认）

所有业务表按同一套规矩建，M2 的 sandboxes 及以后新表照此执行：

1. **`id` 单列代理主键**：uuidv7，应用生成（与 msg_id / turn_id 同族）。不用复合自然键当 PK。
2. **`user_id text` 全表冗余**：无 FK。收益：按用户直查免 JOIN、将来开 PG RLS 策略要求每行自带租户列。子表 user_id **从父行派生**（INSERT 时 SQL 内 SELECT 父表），不靠应用传参，防不一致。依据：e2b 每张业务表都带 team_id。
3. **每组编号用父行计数器**：如 `sessions.last_seq`，`UPDATE … RETURNING` 同事务发放。依据：PG 社区对 per-group 编号的标准方案（sequence 给不了每组编号且回滚留洞；MAX+1 并发报废）。行锁天然串行化同组插入，回滚不留空号，删行不复用号。e2b 的 `envs.spawn_count` 即此模式。
4. **业务唯一性走 UNIQUE 约束**：如 messages 的 `(session_id, seq)`、`(msg_id)`。

## 四类存储的角色

| 存储 | 角色 | 放什么 | 丢了会怎样 |
|---|---|---|---|
| 业务 PG | 事实源（讲座 09 四层模型的 "database"） | sessions / messages / sandboxes | 事故 |
| 文件存储 | 事实源（讲座 09 的 "storage"） | memory / skills / routines / 快照 | 事故 |
| Redis | 协调态（推断：实现选择，讲座只说能力） | 会话队列 / turn 锁 / 变更信号 | 可从 PG 重建，无害 |
| Temporal PG | 执行账本（讲座 16：每步 recorded） | workflow 事件历史 | 应用不查，Temporal 自管 |

## 业务 PG 三表

```
Session
  id uuid PK            -- uuidv7，应用生成
  user_id text          -- 用户维度（无 FK，见"用户维度决策"）
  title text            -- 易读名称，SPA 可设
  last_seq bigint       -- 父行计数器：会话内 seq 发号器（统一约定 §3）
  created_at timestamptz

Message
  id uuid PK            -- uuidv7，应用生成（统一约定 §1）
  session_id uuid       -- FK → Session
  seq bigint            -- 会话内递增（父行计数器发放），对应讲座 15 Ordering
  user_id text          -- 冗余自 sessions.user_id（统一约定 §2）
  kind text             -- user / assistant / tool_call / tool_result
  content jsonb
  msg_id uuid UNIQUE    -- 仅用户消息有；客户端生成，幂等兜底
  turn_id text          -- = Temporal workflowID；仅 assistant 消息有
  created_at timestamptz
  UNIQUE (session_id, seq)

Sandbox
  id uuid PK
  user_id text          -- 归属用户：讲座 09 "one VM per user"，sandbox→user 而非 sandbox→session
  status text           -- creating / awake / asleep / stopped / failed
  vm_ip text            -- agentd 的 Connect-RPC 地址；VM 重建后 IP 会变，必须落库
  snapshot_ref text     -- 指向文件存储 snapshots/{sandbox_id}/
  last_activity_at      -- 空闲计时依据（"5 分钟无活动 → 快照 → 停机"）
```

- **sessions 是什么**：一次会话的元数据。消息、队列、锁、SPA 都按会话组织。
- **messages 是什么**：会话的完整记录，**含 turn 的每一步**（讲座 15 黑条 "Every step is saved to the database"）。msg_id 客户端生成（讲座 15："saves the message locally with a unique ID"），去重 = `INSERT ... ON CONFLICT(msg_id) DO NOTHING`，不需要 Redis 幂等窗。
- **sandboxes 是什么**：VM（"agent 的电脑"）在 server 侧的档案卡，一用户一行。回答四个问题：有没有 VM（行存在与否）、什么状态（status，turn 里首次 tool_call 时决定直接用还是 Resume）、在哪连（vm_ip）、从哪个快照恢复（snapshot_ref）、闲多久了（last_activity_at）。没有它，server 重启 = VM 成孤儿资源。

## 用户维度决策（2026-10-28 确认）

- `sessions`、`sandboxes` 各加 `user_id` 列；**不建 users 表、不加 FK 约束**。
- v1 默认用户 = 配置常量 `local`（intent：第一个且唯一的用户）。**类型用 `text` 不用 `uuid`**：值是常量字符串不是 uuid，强用 uuid 类型反而要塞假值（2026-10-28 用户指出过度设计，已修）。
- 为什么列要现在占住：讲座的基本单位是 user（09 "one VM per user"、08 "understanding of the user"、30 记忆按用户分层）。现在加是两列的成本，以后补是迁移手术。
- `messages` 冗余 `user_id`，从 sessions 父行派生（2026-10-28 二次确认修订：原决策"经 session 继承不冗余"作废——e2b 全表带 team_id，且 PG RLS 要求每行自带租户列）。

## Redis 三个结构（全是协调态）

| 结构 | 是什么 / 作用 | 怎么存 | 为什么这么选 |
|---|---|---|---|
| 会话队列 | 待处理消息缓冲；turn 开始时 drain 成一批（讲座 17 批量规则） | LIST，key `session:{id}:queue` | 瞬时、单消费者、一条命令全取；消息已先落 PG，队列丢了能重建 |
| turn 锁 | 保证"一会话一 turn"（讲座 15）；值存 workflowID，供打断时 Cancel | STRING，`SET NX EX 600` | SETNX 原子抢占；TTL 让崩溃的 worker 自动释放 |
| 变更信号 | "something changed" 空信号（讲座 15 Watching） | Pub/Sub，频道 `changed:{session}` | 协议自带"丢了无害"；SPA 收到后带 seq 回源拉真数据 |

~~幂等窗 `msg:seen:{id}`~~ 已删除：PG `UNIQUE(msg_id)` + `ON CONFLICT` 就是讲座 15 去重的精确实现。

## 文件存储

```
data/users/{uid}/                # ⚠️ 前缀是推断：讲座 08 原图只有裸目录（"Simplified example"）
  memory/profile.md              # 永远在 prompt 里（讲座 30：Profile ≤100 facts / 4000 chars）
  memory/log/2026-10.md          # 带日期的事实，一行一条（讲座 30：Log 30 条注入）
  skills/<name>/SKILL.md         # "Use this when…"（讲座 08）
  routines/<name>                # prompt + 触发时间（v2 才用）
  snapshots/{sandbox_id}/        # 快照文件：内存 + 磁盘
```

- 为什么是文件：讲座 08 "plain files that the agent reads and updates itself"，模型擅长 grep 和编辑文本。
- 为什么在 server：讲座 09 saved state "always on the server"；讲座 11 休眠第 1 步 "Files sync to cloud storage"。
- 前缀说明：`data/users/{uid}/` 是多用户命名空间预留，与 PG 的 `user_id` 列同一逻辑；方向合理但属推断，已标注。
- v1 简化：快照含整个磁盘，文件同步这步可省；目录布局不变。

## 基本流程：发消息到回显

```
== 受理（讲座 15 Sending）==
SPA       → ys-server : POST /send，msg_id 由 SPA 生成
ys-server → PG        : INSERT messages ON CONFLICT(msg_id) DO NOTHING
ys-server → Redis     : RPUSH 会话队列
ys-server → Redis     : SET NX active_turn（值 = workflowID）
                        锁已存在 = 有 turn 在跑 → CancelWorkflow 打断（讲座 17 用户特权）
ys-server → Temporal  : ExecuteWorkflow(TurnWorkflow, session_id)
ys-server → SPA       : 200 + seq

== 执行（讲座 15 The turn）==
ys-worker → Redis     : drain 队列，得到一批消息
ys-worker → LLM       : 组装 prompt 调模型
ys-worker → agentd@VM : 有 tool_call 才调；首次调用前先 Resume VM（讲座 11 懒恢复）
ys-worker → PG        : 每一步写 messages
ys-worker → Redis     : PUBLISH changed（空信号）

== 回显（讲座 15 Watching + Ordering）==
SPA       → ys-server : GET /messages?after_seq=N，按 seq 顺序应用
```

两个关键形状：**消息先落 PG 再进队列**（队列可丢）；**VM 只在 tool_call 时才被唤醒**（讲座 11 "Restored when a tool needs it"）。

## 我的选择 与 暂不实现

我的选择（讲座只说能力、没说实现；v1 照做，但可质疑可推翻）：

- Redis 三个结构的具体选型
- sandboxes 表的存在与列
- turn 锁用 SET NX EX
- 文件同步云存储的时机
- `data/users/{uid}/` 前缀

暂不实现（讲座明确有，但 v1 用不上；不是版本计划，只是停车场）：

- `messages.source` 唤醒来源字段（讲座 10 "每次唤醒记录来源"；v1 只有 web 一个来源，建了是空列）
- routines 表 / 文件的实际调度
- 多用户：users 表 + FK
- LoadHistory 的全量读：每个 turn 都读会话全部消息发给 LLM，会话长了是已知 growth 点——M5 memory 压缩时一并解决，届时这行从停车场划掉
