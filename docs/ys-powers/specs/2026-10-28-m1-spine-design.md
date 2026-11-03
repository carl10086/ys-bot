# M1 主干 设计文档

> 日期：2026-10-28 · 状态：设计已确认，待实现
> 输入：`docs/ys-powers/intent/ys-bot.md`（已确认意图）+ 设计对话五节（2026-10-27 ~ 10-28）
> 引用文档：`docs/architecture/server-vm-boundary.md`、`docs/architecture/data-model.md`（定稿）、`docs/architecture/phasing.md`、`CONTEXT.md`

## 1. Objective（目标）

**跑通 ys-server → ys-worker 的最小主干**：SPA 发消息 → ys-server 去重/入队/触发 → Temporal TurnWorkflow → ys-worker 调真 LLM → 回复落库 → SPA 回显。

M1 是整个系统的脊梁：数据模型、hit-send 协议、turn 语义（锁/打断/批量）、durable execution 全部在这一步验证。**无 VM、无工具、无快照、无记忆。**

验收标准（全部手动可验）：

1. SPA 发一条消息，能看到 LLM 回复出现在消息流里
2. 同一 `msg_id` 重发（网络重试场景），不产生第二条消息
3. turn 进行中再发消息，当前 turn 被打断、合并后开新 turn
4. ys-worker 进程崩溃后重启，进行中的 turn 从断点恢复（Temporal durable 验证）
5. ys-server 重启后，会话和消息历史完好（事实源在 PG）

## 2. Commands（命令）

```bash
# 起依赖（Temporal + PostgreSQL + Redis）
cd deploy && docker compose up -d

# 初始化 schema（sessions + messages 两表；sandboxes 表 M2 再建，M1 用不到）
psql $PGURL -f internal/store/schema.sql

# 跑服务（两个进程，本地或 Linux 服务器均可——M1 无 VM，与平台无关）
go run ./cmd/ys-server   # HTTP API + 内嵌 SPA，:8080
go run ./cmd/ys-worker   # Temporal worker

# 测试
go test ./...
```

环境变量：`DATABASE_URL`、`REDIS_ADDR`、`TEMPORAL_ADDR`、`LLM_API_KEY`（+ `LLM_MODEL` 有默认值）。

## 3. Project Structure（项目结构）

单 module monorepo，一个 `go.mod`，私有代码全在 `internal/`：

```
cmd/ys-server/        HTTP API + 内嵌 SPA（对外入口）
cmd/ys-worker/        Temporal worker（注册 workflow + activities）
internal/agent/       agent loop：fantasy 薄层关在适配接口后，业务代码不得直接 import fantasy 类型
internal/workflow/    TurnWorkflow + activities（drain 队列 / 调 LLM / 写 messages / 发信号）
internal/store/       存储层：PG（sessions/messages）+ Redis（队列/锁/信号）
web/                  Vite + React 最小 SPA：单会话视图（输入框 + 消息流 + 轮询 after_seq）
deploy/               docker-compose.yml（Temporal + PG + Redis）
proto/agentd/         （M2 才建；M1 没有 agentd）
```

已砍：`cmd/ys-dev`（存在前提是 Mac mock 全链路，前提已消失）。

## 4. Code Style（代码风格）

- 最小实现：single-use 场景不抽接口/类；200 行能解决不写 500 行
- 不建 `pkg/`；不拆多 module；不为"将来多节点"提前抽象（Redis 目录路由、client-proxy 都不做）
- 改动外科手术化：每行变更可追溯到需求，不顺手"改善"邻近代码
- 数据归属红线：事实源只写 PG/文件存储；Redis 只放协调态（队列/锁/信号）
- proto 改动后必须 `buf generate`，产物在 `internal/gen/` 不手改（M2 起适用）

## 5. 关键设计（已定稿，实现时照此）

数据模型细节以 `docs/architecture/data-model.md` 为准，此处只列实现要点：

- **去重**：`INSERT ... ON CONFLICT(msg_id) DO NOTHING RETURNING seq`——一次 SQL 完成幂等，无 Redis 幂等窗
- **顺序**：`seq` 会话内递增（`COALESCE(MAX(seq),0)+1` 在事务内或 PG sequence per session 实现，实现时选简单的）；SPA `GET /messages?after_seq=N` 轮询回显
- **队列**：Redis LIST `session:{id}:queue`；**消息先落 PG 再入队**，队列可丢
- **turn 锁**：`SET session:{id}:active_turn <workflowID> NX EX 600`；锁已存在 = 有 turn 在跑 → `CancelWorkflow` 打断 → 合并队列 → 起新 turn
- **TurnWorkflow**：drain 队列得一批消息 → activity 调 LLM（fantasy）→ 每步写 `messages`（kind=user/assistant）→ PUBLISH `changed:{session}` 空信号
- **workflowID**：`turn:{sessionId}:{uuid}`，即 `turn_id`
- **LLM**：fantasy 薄层接真实 provider（`LLM_API_KEY`）；M1 的 agent loop 就是一次普通补全，无工具循环

## 6. Testing Strategy（测试策略）

- **单测重点**：store 层（去重 ON CONFLICT、seq 递增）、队列 drain、锁的抢占/打断分支
- **链路冒烟**：手动跑验收标准 1–5（写进 README 的验证清单）
- **不写**：E2E 框架、mock LLM 的复杂夹具——M1 用真 LLM 真链路，测试贵在少而准
- mock 驱动只作为 vmdriver 接口的测试替身存在（M2 才需要）

## 7. Boundaries（边界 / 不做）

- ❌ VM、工具、agentd、快照、记忆文件（M2–M5）
- ❌ 多用户：users 表、FK（user_id 列已占，常量 `local`）
- ❌ `messages.source` 字段、routines、唤醒流水线的另外 6 个源
- ❌ SPA 美化：多会话管理、流式渲染（轮询就够）
- ❌ 浏览器/桌面进 VM（v1 明确排除）
- ❌ 往 Redis 写永久数据

## 8. Open Questions（实现时回答）

- LLM provider 选哪家、`LLM_MODEL` 默认值（实现第一天定，写进 README）
- seq 递增的具体实现（事务内 MAX+1 vs per-session sequence）——选简单且正确的，代码里注释
- 打断时 Temporal Cancel 的清理语义（已写一半的消息算不算数）——实现时按"已落库的保留"处理并验证
- Linux 服务器到位时间（决定 M2 用 mock 还是直接真 Firecracker）
