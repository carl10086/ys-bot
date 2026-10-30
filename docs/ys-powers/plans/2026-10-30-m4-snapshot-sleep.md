# M4 实施计划：快照休眠/恢复

> 依据：`docs/ys-powers/specs/2026-10-30-m4-snapshot-sleep-design.md`（2026-10-30 定稿）。
> 术语以 `CONTEXT.md` 为准。工作区：直接基于 main（用户选定）。

## 依赖图

```
T1 PG 查询/索引 ──┬──► T2 vmhostd 休眠/恢复全路径 ──► T5 自愈+小修
                 ├──► T3 续期写入点
                 └──► T4 SleepWorkflow + Schedule（也依赖 T2 的 SleepVM RPC）
全部 ──► T6 152 部署 + e2e 验收
```

T2 是整个 M4 的风险中心（真 Firecracker 快照行为），故意排在 workflow 之前：
先在 152 手动 smoke 验证 pause/snapshot/load/resume 真实表现，再让 Temporal 驱动它。

## Task 1：PG 层——索引 + 四个新查询

- **改动**：
  - `schema.sql`：`CREATE INDEX IF NOT EXISTS sandboxes_awake_idle ON sandboxes (status, last_activity_at)`；
    status 注释词表 `asleep` → `sleeping`
  - `queries.sql`（sqlc 生成，不手改 gen）：
    - `RefreshSandboxActivity :exec` — `UPDATE sandboxes SET last_activity_at = now() WHERE id = $1`
    - `ListSleepCandidates :many` — `WHERE status='awake' AND last_activity_at < now() - $1::interval`（跨宿主，sweeper 用）
    - `TryStartSleep :execrows` — CAS：`SET status='sleeping' WHERE id=$1 AND status='awake' AND last_activity_at < now() - $2::interval`
    - `SetSandboxSnapshotRef :exec` — 写/清 `snapshot_ref`
  - `ReleaseSandbox` 追加 `snapshot_ref = NULL`（销毁/对账清场时快照引用一并清）
- **测试**（`internal/store/pg`，YS_TEST_PG 库）：CAS 并发两写只赢一个；过期边界（刚好过期/未过期/非 awake）；Refresh 生效
- **验收**：`go test ./internal/store/pg/` 绿且 SKIP=0；152 PG 上 ApplySchema 幂等（列已存在，只建索引）
- **Files**：`internal/store/pg/schema.sql`、`queries.sql`、`gen/`（生成）、`pg_test.go`

**CP1 ✅：pg 测试全绿。**

## Task 2：vmhostd 休眠/恢复全路径（风险中心，先行验证）

- **proto**：`vmhostd.proto` +`rpc SleepVM(SleepVMRequest) returns (SleepVMResponse)`；`buf generate`
- **fc.go**：`put` 之外加 `patch` 方法；+`pauseVM`（PATCH /vm Paused）、`createSnapshot`（PUT /snapshot/create，
  `mem_file_path=snap.mem`、`snapshot_path=snap.vmstate`）、`loadSnapshot`（PUT /snapshot/load + PATCH /vm Resumed）
- **vm.go**：
  - `sleepVM`：pause → createSnapshot → 杀 fc → 拆 tap（**目录/rootfs/快照保留**）→ `SetSandboxSnapshotRef`
  - `ensureVM` 加 resume 分支：非 awake 且 `snap.vmstate` 存在 → **复用 `sb.Slot`**（不重新分配）建同名 tap →
    起 fc → loadSnapshot → waitAgentd → `SetSandboxAwake`；无快照走现有 boot 路径
  - `reconcile`：`ListActiveSandboxesByHost` 查 awake/creating 不变（sleeping 天然不在结果里=跳过）；
    新增：sleeping 但 `snap.vmstate` 不在 → 转 `stopped`（不动目录其余文件，记日志）
  - resume 分支防御：sleeping 但 pid 进程还在（休眠中途崩溃）→ 先杀进程再 load
- **server.go**：`SleepVM` handler（parseSandboxID → sleepVM，日志沿用 dur 叙事）
- **测试**：reconcile 分类抽纯函数单测（不碰 DB）；fc 调用无法在 Mac 单测，靠下方 smoke
- **152 手动 smoke（本任务的真正验收）**：boot 一个 VM → exec `touch /root/marker` → curl SleepVM →
  确认进程消失+快照文件在 → curl EnsureVM → 确认 resume（同 IP）→ exec `ls /root/marker` 文件还在
- **Files**：`proto/vmhostd/v1/vmhostd.proto`、`internal/gen/`（生成）、`internal/vmhostd/fc.go`、`vm.go`、`server.go`、`vm_test.go`（或 net_test.go 同包）

**CP2 ✅：152 手动 smoke 通过——快照 round-trip 真实可行后才继续。若 Firecracker 实际行为与设计冲突，回 spec 修订。**

## Task 3：续期写入点

- **hitsend.Submit**：成功落库后（含 deduped 重发——用户确实按了发送）按 session→user→sandbox 链
  刷新 `last_activity_at`；无 sandbox 行（从未用过 VM）则跳过
- **workflow.ExecTool**：activity 开头按 `in.SandboxID` 刷新（在跑的长命令期间 turn 持续续期，
  这是 never-mid-turn 成立的前提）
- **测试**：hitsend 测试库验证刷新；ExecTool 用 fakeDriver 验证
- **Files**：`internal/hitsend/hitsend.go`、`internal/workflow/activities.go`、对应 `_test.go`

## Task 4：SleepWorkflow + sweeper + Schedule 注册

- **vmdriver**：`Driver` 接口**纯新增** `SleepVM(ctx, sandboxID) error`（RemoteDriver 复用 vmhostClient 转发；
  fakeDriver 同步实现）。⚠️ 计划级细化：spec 说"vmdriver 不改"指的是 boot/resume 分支对调用方透明；
  SleepVM 是休眠链路的新调用，走 Driver 接口是唯一不复制注册表逻辑的路径——**请评审确认**
- **workflow 包**：
  - `SleepSweepWorkflow`：activity `ListSleepCandidates(N)` → 对每个 sandbox 启动 `SleepWorkflow`
    （workflowID=`sleep:<sandbox_id>`，Temporal 按 ID 去重并发）
  - `SleepWorkflow`：activity CAS（`TryStartSleep`，带 N 复核）→ 0 行直接结束 → activity `SleepVM`（经 Driver）→
    失败则 activity 回滚 `SetSandboxStatus('awake')`
  - N 从 env `SLEEP_IDLE_MINUTES`（默认 10）读入，作 activity 参数透传
- **cmd/ys-worker/main.go**：注册两个 workflow；启动时 `ScheduleClient().Create`（ID=`sleep-sweeper`，
  interval=1min，overlap=Skip，`AlreadyExists` 视为成功）——多节点幂等
- **测试**：SleepWorkflow 的 CAS 失败短路 / SleepVM 失败回滚，用 fake Driver + 测试库覆盖
- **Files**：`internal/vmdriver/vmdriver.go`、`internal/workflow/sleep.go`（新）、`cmd/ys-worker/main.go`、测试

**CP3 ✅：`go test ./...` 全绿 SKIP=0；`-race` 通过。**

## Task 5：Exec 自愈 + turn.go Sprintf 修复

- **自愈落位（计划级细化，请评审确认）**：放在 `RemoteDriver.Exec` 内部而非 ExecTool activity——
  收到 `connect.CodeFailedPrecondition` → 调一次 `EnsureVM`（resume）→ 重试 Exec 一次，再失败才报错。
  理由：所有 Exec 调用方受益，activity 代码不动；与 spec "最多一次"约束一致
- **turn.go:120**：`fmt.Sprintf(`{"exit_code":-1,...}`)` 改 `json.Marshal`（stderr 含引号时现在会生成非法 JSON）
- **测试**：fake vmhostd client 模拟 FailedPrecondition→恢复 序列；Sprintf 修复加引号 stderr 用例
- **Files**：`internal/vmdriver/vmdriver.go`、`internal/workflow/turn.go`、测试

## Task 6：152 部署 + e2e 验收（spec 验收 1–5）

- 部署 vmhostd：GOOS=linux GOARCH=amd64 build → scp → rescue env（/proc/pid/environ，不打印值）→ kill+mv+nohup
- e2e（临时把 `SLEEP_IDLE_MINUTES=1` 加速观察）：
  1. SPA 发消息用工具 → 等休眠（进程消失、status=sleeping、快照文件在、slot 保留）
  2. 再发消息触发工具 → 原地恢复、休眠前建的文件还在、turn 完成
  3. 长 turn 期间观察 sweeper 日志：CAS 抢不到（续期生效）
  4. 删掉 snap.vmstate 后发消息 → 日志见 resume 失败 → 自动全新 boot → turn 完成
  5. 重启 vmhostd → sleeping 目录完好；再删快照重启 → 转 stopped
- 收尾：spec 状态行改为"已实现"；CLAUDE.md/CONTEXT.md 如有漂移顺手对齐

**CP4 ✅：验收 1–5 全过，e2e 证据（日志/curl 输出）贴进 spec 或提交信息。**

## 风险与回退

| 风险 | 缓解 |
|---|---|
| Firecracker 快照在现有 PVM 内核/rootfs 上行为未知 | T2 先手动 smoke，最坏情况回 spec 改设计，workflow 层一行未写 |
| Schedule 重复注册 | 固定 ID + AlreadyExists 容忍；overlap=Skip |
| 休眠中用户发消息 | 不需要防：turn 走 EnsureAwake → resume；这正是设计 |
| vmhostd 无鉴权红线 | 本计划不扩大暴露面（SleepVM 与既有 RPC 同端口同级别）；红线独立任务处理 |
