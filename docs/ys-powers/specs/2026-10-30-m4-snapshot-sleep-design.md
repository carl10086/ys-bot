# M4 设计：快照休眠/恢复

> 状态：2026-10-30 访谈定稿（Q1–Q4 + 存储议题，全部用户确认）；**2026-10-30 已实现**（验收 1–5 全过，见下）。
> 上游文档：`CONTEXT.md`（术语）、`docs/adr/0001-snapshot-on-host-local-disk.md`、`docs/architecture/phasing.md` M4 行、
> Grok Bot 讲座 s12/s15（`docs/reference/cs146s-grokbot-lecture.html`）、参照物 `ys-learn/refer/e2b-runtime`。

> **实现期修订（e2e 踩出）**：
> 1. sweeper 起 SleepWorkflow 从 child workflow 改为 activity 里 `client.ExecuteWorkflow`——实测"父 workflow
>    同一 task 内发起 child + 立即完成"时 child 从未被 server 创建（Initiated 有、Started/Failed 无）。
> 2. Schedule 重复创建的错误是 `temporal.ErrScheduleAlreadyRunning`（不是 serviceerror.AlreadyExists）。
> 3. spawnFC 必须先删残留 fc.sock（Firecracker v1.17 不 unlink，bind 失败进程直接退出）。
> 4. resumeVM 失败时把 console.log 尾部带进错误（目录随即被降级删除，那是唯一现场）。
> 5. e2e 期间发现 GoLand 残留旧 ys-server/ys-worker 会静默分流（poll 同 task queue / 占 :8080）——
>    e2e 前必须确认只有一个 server/worker 在跑。
>
> **验收证据（152，2026-10-30）**：① 无活动 ~2min 后自动 sleeping，进程消失，snap.mem(512MB)+snap.vmstate 落盘，
> slot 保留，sleep dur≈2.8s ② 发消息触发 tool call → resume 同 IP（10.200.0.2）原地恢复，休眠前建的
> /tmp/m4-e2e.txt 内容 "alive" 完好 ③ turn 进行中的 sweep 候选=0（续期生效）④ 删 snap.mem 后 resume 失败
> → 日志"resume 失败，降级全新 boot" → turn 正常完成 ⑤ sleeping 下重启 vmhostd 目录完好不动；
> 删快照重启 → "快照丢失 → stopped"，slot/snapshot_ref 清空；再发消息走全新 boot 正常完成。

## Objective（目标与验收）

**目标**：空闲 VM 自动快照休眠、停机省钱；下次 tool call 从快照原地恢复，环境还在。
这是 ys-bot 选"always-on agent + Firecracker"架构的根本原因（intent 文档原文），
对应 Grok Bot 讲座 s12：*"Going to sleep (a few minutes after the last activity, never mid-turn)"*。

**验收标准**（每条都要可观测地跑通）：

1. 无活动 N 分钟后（N 默认 10min），VM 自动休眠：firecracker 进程消失、`sandboxes.status='sleeping'`、
   VM 目录下存在 `snap.mem` + `snap.vmstate`、slot/VMIP 保留
2. 休眠后发消息触发 tool call → VM 从快照原地恢复（同 slot 同 IP），**休眠前在 VM 里创建的文件仍在**，
   turn 正常完成
3. never mid-turn：长 turn（持续工具调用）期间 sweeper 绝不动它；休眠完成后才允许入睡
4. 人为损坏快照文件后发消息 → resume 失败自动降级为模板全新 boot，turn 正常完成，用户无感
5. vmhostd 重启后 reconcile 不清场 sleeping VM 的目录；若快照文件丢失（磁盘故障）则转 `stopped`，
   下次懒唤醒走全新 boot

**明确不做**（写死，防 scope 蔓延）：

- diff 增量快照、多版本快照（只留最新一份，每次休眠覆盖写）
- 快照上传对象存储、跨宿主 resume（e2b 路线，规模化后再议；演进路径见 ADR-0001）
- files sync to cloud storage（Grok Bot 休眠第 1 步；属独立特性，M5 记忆文件时再议）
- resume prefetch、fsfreeze、deferred CoW export（e2b 的重型机制，单机过度设计）
- 休眠老化策略（睡 X 天删快照释放 slot——slot 紧张时再加）
- vmhostd 鉴权（已知红线，独立任务，不在本 spec）

## 设计决策（访谈定案，不再开放）

| # | 议题 | 决策 |
|---|---|---|
| Q1 | 谁计时触发休眠 | `sandboxes.last_activity_at`（TIMESTAMPTZ，最近活动时间，M2 预留列启用）+ 续期写入点（`hitsend.Submit` 与 `ExecTool` 各置 `now()`）+ Temporal Schedule 每分钟点火 sweeper（固定 ID `sleep-sweeper`、ys-worker 启动时幂等注册、overlap=Skip）；过期判据 = `last_activity_at < now() - N`，N 是纯配置不在写路径里 |
| Q2 | never-mid-turn 互斥 | 休眠入口 = 原子 CAS：`UPDATE sandboxes SET status='sleeping' WHERE id=$1 AND status='awake' AND last_activity_at < now() - $2`，0 行即放弃。续期即互斥：在跑的 turn 持续续期，CAS 抢不到。残余竞态靠自愈：`ExecTool` 收到 VM 非 awake 的 `FailedPrecondition` → 调 EnsureVM（resume）后重试，不报错。**硬约束：N > 单工具 timeout（当前 shell=60s，N=10min 满足）** |
| Q3 | 状态机 | 新增 `sleeping` 态；sleeping 期间 slot/VMIP 保留（guest 网络配置冻在内存快照里，resume 必须同 tap 同 IP）；reconcile 跳过 sleeping（快照丢失 → `stopped`）；resume 失败自动降级全新 boot（打日志计数，不暴露给用户）；删会话不动 sandbox（per-user 共享） |
| Q4 | RPC 形状 | proto 只加 `rpc SleepVM`；`EnsureVM` 签名语义不变，vmhostd 内部按"snap.vmstate 是否存在"分 resume/boot 两支；**vmdriver 一行不改**；fc.go 加 `pauseVM`/`createSnapshot`/`loadSnapshot`（需新增 PATCH 方法） |
| Q5 | 快照存储 | 宿主本地盘（`{VMDir}/<sandbox_id>/`）；PG 只记元数据。理由与演进路径见 ADR-0001 |

**schema 调整**（M2 已预留 M4 列，**直接启用，不增删列**——2026-10-30 用户定案）：

- `last_activity_at timestamptz`：启用为"最近活动时间"，`Submit`/`ExecTool` 续期置 `now()`；
  建 `(status, last_activity_at)` 索引供 sweeper
- `snapshot_ref text`：启用为快照引用标记——休眠成功时写入快照路径，降级 boot / 销毁后清空（NULL = 无快照）
- status 词表注释 `asleep` → `sleeping`（与 CONTEXT.md 统一）
- 落地方式：152 的 PG 已建表且列已存在，只需 ApplySchema 支持幂等 `CREATE INDEX IF NOT EXISTS`，无需 ALTER 无需 reset

## Tech Stack

Go（monorepo）、Temporal Go SDK（Schedule API、workflow/activity）、PostgreSQL（sqlc 代码生成）、
Connect-RPC（buf 生成）、Firecracker HTTP API（unix socket，fc.go 最小客户端）。

## Commands

```bash
# proto 变更后（产物在 internal/gen/，不手改）
buf generate

# PG 查询变更后（internal/store/pg/queries.sql → gen/）
sqlc generate

# 全量测试（三个 PG 测试库，缺一个 env 整包静默 SKIP——SKIP 数必须为 0）
YS_TEST_PG=...@152.136.212.59:15432/ys_sandbox_test \
YS_TEST_PG_API=...@.../ys_sandbox_test_api \
YS_TEST_PG_WF=...@.../ys_sandbox_test_wf \
YS_TEST_REDIS=152.136.212.59:16379 REDIS_PASSWORD=... \
go test ./...

# e2e 验收依赖 152 真 Firecracker；vmhostd 部署沿用 M2 做法：
# GOOS=linux GOARCH=amd64 build → scp → 152 上 rescue env（/proc/<pid>/environ，不打印值）→ kill+mv+nohup
```

## Project Structure（改动落点）

| 文件 | 改动 |
|---|---|
| `proto/vmhostd/v1/vmhostd.proto` | +`rpc SleepVM` |
| `internal/vmhostd/fc.go` | +PATCH 支持；+pauseVM/createSnapshot/loadSnapshot |
| `internal/vmhostd/vm.go` | ensureVM 加 resume 分支（建同名 tap → 起 fc → load → resumed → waitAgentd）；+sleepVM（pause → snapshot → 杀 fc → 拆 tap，目录与 rootfs 保留）；reconcile 跳过 sleeping、快照丢失转 stopped |
| `internal/store/pg/schema.sql` / `queries.sql` | schema 调整（见上表）；+CAS、+续期、+过期扫描三个查询 |
| `internal/hitsend/hitsend.go` | Submit 成功路径续期 last_activity_at |
| `internal/workflow/` | +SleepSweepWorkflow、+SleepWorkflow（CAS → SleepVM → 失败回滚 awake）；ExecTool 续期 + FailedPrecondition 自愈重试；turn.go:113 Sprintf 拼 JSON 顺手修为 json.Marshal |
| `cmd/ys-worker/main.go` | 启动时幂等注册 Schedule `sleep-sweeper` |
| `internal/vmdriver/vmdriver.go` | **不改**（分层验证点） |

## Code Style

沿用 CLAUDE.md 与现有代码约定：中文注释写"为什么"（参考 fc.go/vmhostd 头注释密度）；
函数短、单一职责；vmhostd 保持哑基础设施（零 LLM/workflow/会话概念）；
PG 查询一律走 queries.sql + sqlc，不手写 SQL 字符串。

## Testing Strategy

- **单测**（三个测试 PG 库，SKIP=0 才算过）：
  - pg 层：CAS 占坑（并发两写只赢一个）、续期、过期扫描的边界（刚好过期/未过期/非 awake）
  - hitsend：Submit 后 last_activity_at 被刷新
  - reconcile 分支逻辑：awake 无进程 → 清场；sleeping → 跳过；sleeping 无快照文件 → stopped
- **152 实测**（e2e，验收 1/2/4/5 的唯一裁判）：boot → exec 建标记文件 → 等休眠（可把 N 临时调小）→
  确认进程消失+快照文件在 → 发消息触发工具 → 文件还在、turn 完成
- ** Temporal 层**：SleepWorkflow 的 CAS 失败/回滚路径用 activity 级测试覆盖（fake driver）

## Boundaries

- **Always**：中文注释；快照只留最新一份（覆盖写）；休眠/恢复全程每步有日志（dur 叙事风格沿用）
- **Ask first**：N 默认值调整；新增本 spec 未列的 PG 列；引入新依赖；Temporal Schedule 参数（间隔/overlap）变更
- **Never**：快照字节进 PG 或 Redis；vmhostd 出现 session/turn/LLM 概念；绕过 CAS 直接 UPDATE status；
  ExecTool 自愈做成无限重试（最多一次：EnsureVM → 重试 Exec → 再失败则报错）

## Success Criteria

= Objective 的验收 1–5 全部在 152 真机跑通 + `go test ./...` 全绿且 SKIP=0 + -race 通过。

## Open Questions

- N 默认值建议 10min（Grok Bot "a few minutes"；其 summarize pass 也在回合后 ~10min）。如无异议即此值。
- reconcile 目前只在 vmhostd 启动时跑一次；v1 不引入周期 reconcile（快照丢失只在重启时被发现）。
  如无异议即此边界。
