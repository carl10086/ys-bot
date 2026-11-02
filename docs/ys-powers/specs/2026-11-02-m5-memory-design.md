# M5 设计：记忆与技能（memory/skills）

> 状态：2026-11-02 访谈定稿（存什么 → 怎么用 → 存储选型，全程对齐 Grok Bot 讲座与四个开源系统调研）；**2026-11-02 已实现**（验收 1–7 全过，见下）。
> 上游文档：`CONTEXT.md`（M5 术语）、`docs/adr/0002-memory-in-pg-not-files.md`、
> Grok Bot 讲座 §08/§18/§30（`docs/reference/cs146s-grokbot-lecture.html`）、
> 调研对比页（OpenViking / OpenClaw / Mem0 / Hermes 的提取-存储-检索设计）。
>
> **实现期修订（e2e 踩出）**：
> 1. **工具校验/用法错误不能是 activity error**——Temporal 默认无限重试会把 turn 卡死（实测模型发缺参
>    `memory_save`，turn 卡到 Attempt 7+）。校验/NotFound 一律返回 `exit_code=-1` 的模型可见输出
>   （模型看到会自己纠错，实测即自愈）；只有基础设施错误（PG 挂）才配当 activity error。
>    `ExecTool` 同类 M2 遗留一并修。
> 2. MemoryPassWorkflow 的 activity 重试**封顶 3 次**——每次重试都是一次真实 LLM 调用（token 成本），
>    持续性失败重试无益（pass 本就是优化）。
> 3. `MEMORY_BASE_URL` 独立 env（缺省回落 LLM_BASE_URL）——验收 6 需要只打残提取链路；
>    也为将来提取模型走不同网关留了口。
>
> **验收证据（本地 server+worker + 152 PG，2026-11-02；全程不需 VM）**：
> ① 会话 A 说偏好 → 新会话 B 回复"你是 Go 后端工程师。你偏好简短直接的回答……"
> ② `memory_ops` 有 add（source=agent）与 delete（source=pass:turn:…）审计行
> ③ 更正偏好：agent 先 add 新文本 → pass 裁决 delete 旧行 → 终态一行不并存
> ④ `memory_save` applied=1 立即落库 ⑤ 真模型调 `memory_search("回答")` 命中并正确转述
> ⑥ MEMORY_BASE_URL 指死端口：ExtractMemories 重试 3 次放弃，turn 回复完全正常
> ⑦ note ttl=1min：expires_at 落库，过期后活跃查询=0（不进注入）

## Objective（目标与验收）

**目标**：agent 跨会话记住用户的偏好与事实、沉淀可复用技能——Grok Bot 四层记忆（Profile/Log/Notes/Everything-else）+ skills 树，**全部落 PG 三表**。
对应 phasing.md M5 行："memory/skills 文件树 + prompt 注入 + turn 后 memory pass"（文件树形态已在 ADR-0002 改判为 PG）。

**验收标准**（每条可观测）：

1. 告诉 agent 一个偏好 → turn 结束后 pass 提取落库 → **新建会话**发消息，回复体现该偏好（"新会话里它还记得"，phasing 验收原文）
2. turn 结束后 `memory_ops` 审计有记录：op 类型、before/after、source=pass:<turn_id>
3. 偏好变更（"以后别这样了"）→ update/delete 旧条目，**不并存两条矛盾记忆**
4. agent 调 `memory_save` 主动保存 → 立即落库，审计 source=agent
5. `memory_search` 能命中注入窗口（profile 100 条 / log 30 条）之外的旧条目；`memory_read` 能读 skill 全文
6. pass 失败（LLM 出错）→ turn 回复不受影响，错误只记日志
7. note 过 `expires_at` 后不再出现在注入里

**明确不做**（写死，防 scope 蔓延）：

- 向量检索 / embedding（schema 预留演进路径，不实现；v1 检索 = SQL 关键词）
- importance 衰减排序、MMR 多样性（importance 字段照存，排序只用 importance DESC + 时间）
- 后台整理合并（OpenClaw dreaming / OpenViking ov compile——出现真实臃肿 bad case 再做）
- SendToUser 工具（"文本即回复"语义不变；属事件唤醒时代的前置改造，已进 CONTEXT.md 停车场）
- routines / 事件唤醒源（phasing 明确推迟）
- 记忆的 SPA 查看/编辑界面（调试走 SQL）
- **会话历史压缩（compaction）**：与记忆是两个问题——记忆管跨会话，压缩管单会话装不下 context window。当前 `LoadHistory` 全量加载（`ListMessages limit=0`），M5 不动；将来做压缩时 memory pass 可复用为 pre-compaction flush 的载体（OpenClaw 模式）。M6 候选
- 多用户（user_id 沿用常量 'local'，但表结构带 user_id 列）

## 设计决策（访谈定案，不再开放）

| # | 议题 | 决策 |
|---|---|---|
| Q1 | 存什么 | Grok Bot 全量：profile + log + note + skills。原始档案 = 既有 `messages` 表，不重做。"记忆是信息不是指令"：事实进 memories，流程进 skills |
| Q2 | 存哪 | **PG 三表**（memories / skills / memory_ops），ADR-0002。VM 完全不参与：记忆服务端可读可写，VM 睡着/不起 sandbox 的简单对话都不受影响 |
| Q3 | 谁来写 | **服务端 memory pass 为主路径**（Grok Bot/OpenViking/mem0 同派，可靠可审计）+ agent 显式工具 `memory_save` 为辅 |
| Q4 | pass 时机 | **每 turn 结束后异步**：activity 里 `client.ExecuteWorkflow`（M4 实测坑：不用 fire-and-forget child workflow）；失败不阻塞回复；秒级可见性窗口 v1 接受 |
| Q5 | 怎么用·注入 | 每 turn BuildSystemPrompt：profile 全量（≤100 条/4000 字符截断）+ log 最近 30 + 未过期 note + skills 清单（name+description），替代静态 LLM_SYSTEM |
| Q6 | 怎么用·工具 | `memory_search` / `memory_read` / `memory_save` 三个**服务端执行**工具（不经 agentd）——M5 新增第二条工具执行路径 |
| Q7 | 提取管线 | 先读现有记忆全量 → UUID→序号映射（防 LLM 抄错 ID，mem0 技巧）→ 单次 LLM 调用产出 add/update/delete/none 操作 → 事务应用 + hash 去重兜底 + 逐条审计 |

**schema**（新三表；user_id 为 text 沿用 'local' 约定）：

```sql
CREATE TABLE memories (
  id          uuid PRIMARY KEY,
  user_id     text NOT NULL,
  type        text NOT NULL,           -- profile | log | note
  text        text NOT NULL,           -- markdown 一条事实
  importance  int  NOT NULL DEFAULT 5, -- 提取时 LLM 打分 1-10，排序用
  hash        text NOT NULL,           -- md5(text)，精确去重兜底
  expires_at  timestamptz,             -- 仅 note：消退时间
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE skills (
  id          uuid PRIMARY KEY,
  user_id     text NOT NULL,
  name        text NOT NULL,
  description text NOT NULL,           -- 注入清单用，agent 据此决定读不读全文
  content     text NOT NULL,           -- markdown 全文
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE memory_ops (              -- 审计：每次写操作留痕
  id          bigserial PRIMARY KEY,
  user_id     text NOT NULL,
  op          text NOT NULL,           -- add | update | delete
  target_type text NOT NULL,           -- memory | skill
  target_id   uuid,
  before_text text,
  after_text  text,
  source      text NOT NULL,           -- pass:<turn_id> | agent
  created_at  timestamptz NOT NULL DEFAULT now()
);
```

提取 prompt 规则骨架（写进代码注释与实现）：

- 只记值得跨会话保留的：偏好、事实、事件、教训；闲聊不记
- 与现有记忆语义重复 → none；信息更全 → update（保留编号）；直接矛盾 → delete 旧 + add 新
- 流程性沉淀（"以后这类事这样做"）→ 写 skill 不写 memory
- 相对时间落成绝对日期（mem0 范式："上周" → 具体日期）

## Tech Stack

Go（monorepo）、Temporal（MemoryPassWorkflow + activities）、PostgreSQL（sqlc 代码生成）、
现有 LLM 客户端（agent 包）+ `MEMORY_MODEL` env 指定提取用的便宜模型。
proto 不变（memory 工具服务端执行，不经 agentd）；Firecracker/vmhostd 不动。

## Commands

```bash
# PG 查询变更后（internal/store/pg/queries.sql → gen/）
go run github.com/sqlc-dev/sqlc/cmd/sqlc@latest generate

# 全量测试（五个 PG 测试库沿用 M4 配置，SKIP 数必须为 0）
YS_TEST_PG=...@152.136.212.59:15432/ys_sandbox_test \
YS_TEST_PG_API=...@.../ys_sandbox_test_api \
YS_TEST_PG_WF=...@.../ys_sandbox_test_wf \
YS_TEST_PG_HS=...@.../ys_sandbox_test_hs \
YS_TEST_PG_VD=...@.../ys_sandbox_test_vd \
YS_TEST_REDIS=152.136.212.59:16379 REDIS_PASSWORD=... \
go test ./...

# e2e 不依赖 VM：本地起 ys-server + ys-worker + 真 LLM 即可验收
# 发消息 → 等 pass（看 worker 日志）→ 查库 → 新会话发消息 → 看回复体现偏好
```

## Project Structure（改动落点）

| 文件 | 改动 |
|---|---|
| `internal/store/pg/schema.sql` | +memories / skills / memory_ops 三表 |
| `internal/store/pg/queries.sql` + `pg.go` | 注入四查询（profile 全量 / log 最近 30 / 活跃 note / skills 清单）+ ops 应用（add/update/delete，memory 与 skill 两组）+ hash 查重 + memory_search（v1 关键词）+ InsertMemoryOp 审计 |
| `internal/workflow/memory.go`（新） | MemoryPassWorkflow：加载 turn 消息 + 现有记忆 → 提取 → 事务应用 ops + 审计 |
| `internal/workflow/activities.go` | +StartMemoryPass activity（client.ExecuteWorkflow，M4 同款模式）；+memory 三工具的服务端实现 |
| `internal/workflow/turn.go` | turn 尾部触发 StartMemoryPass（回复落库后，异步，失败忽略）；system prompt 从静态改为 BuildSystemPrompt 注入 |
| `internal/agent/agent.go` | system 字段从固定字符串改为每 turn 构建（注入点） |
| `cmd/ys-worker/main.go` | 注册 MemoryPassWorkflow；+`MEMORY_MODEL` env（缺省回落主模型） |

## Code Style

沿用 CLAUDE.md 与既有约定：中文注释写"为什么"；PG 查询一律 queries.sql + sqlc；
ops 应用必须事务 + 审计成对出现；workflow 起子任务走 activity 里 client.ExecuteWorkflow（M4 定例）。

## Testing Strategy

- **pg 层**（用既有 ys_sandbox_test 库）：三表 CRUD、ops 应用三种操作、hash 去重、note 过期过滤、审计写入
- **workflow 层**（ys_sandbox_test_wf）：MemoryPass happy path（fake LLM 返回固定 ops → 断言库内结果 + 审计）；
  update/delete 裁决正确应用；LLM 失败时 turn 不受影响（验收 6）
- **注入**：BuildSystemPrompt 的截断（profile 超 100 条/4000 字符）、log 只取 30、过期 note 不出现、skills 只注入清单不带全文
- **e2e**（本地 + 真 LLM，不需 152）：验收 1/3/4/5 走真模型跑通；验收 2/7 查库断言

## Boundaries

- **Always**：中文注释；ops 应用事务 + 审计成对；pass 异步失败只记日志；记忆查询/写入全走 queries.sql
- **Ask first**：schema 加列（含 embedding 预留列真正启用）；`MEMORY_MODEL` 默认值；新增 agent 工具；profile/log 注入上限调整
- **Never**：记忆/skills 文件进 VM；pass 同步阻塞回复；绕过 ops 直接裸 UPDATE memories；agent 工具直连 PG（必须经 workflow 服务端工具路径）

## Success Criteria

= 验收 1–7 全部跑通 + `go test ./...` 全绿 SKIP=0 + `-race` 通过。

## Open Questions

- profile 写满（>100 条）时：v1 注入侧截断 + 依赖 pass 的 update/delete 瘦身；是否学 Hermes"写满报错倒逼合并"留作运营后的真实 bad case 再定。
- 连续 turn 时两个 pass 可能重叠写库：v1 靠事务 + hash 去重兜底；要严格可改 per-user WorkflowID 串行，留作优化。
- `MEMORY_MODEL` 的具体选型（哪家的便宜档）在实现期按现有 LLM 配置定，不定死。
