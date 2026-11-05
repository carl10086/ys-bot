# Spec: TurnWorkflow 的事件历史成本（turn-history-cost）

> 关联意图：`docs/ys-powers/intent/turn-history-cost.md`（2026-11-05 确认）
> 分支：`feat/turn-history-cost` · 状态：待实现

## Objective

让 `TurnWorkflow` 的 Temporal event history **只记录不可重建的决策**，不记录数据快照。

具体到当前实现的两处浪费（2026-11-05 实测，20 步 turn、会话自带 10×10KB 历史）：

| 现象 | 基线 |
|---|---|
| `LoadHistory` 把整份对话作为 `CallLLM` 的 activity 入参，每次调用都写进 `ActivityTaskScheduled` | 首次 106 KB → 末次 312 KB，**累计 4.29 MB/turn** |
| 每写一条消息就单独发一次 `PublishChanged`（Redis 空信号，可丢） | 41 次 activity、每次 3 个事件 |

用户：你自己（唯一用户）。成功 = 长会话不再逼近 Temporal 的 2 MB payload / 4 MB transaction 上限，且 turn 的 replay 成本与会话长度无关；同时把 turn 的 activity 重试语义收敛到官方最佳实践（有界重试、副作用不重试、长调用可取消）。

### 验收标准（全部可测）

1. `CallLLM` 的 activity 入参只剩 `SessionID`（实测 < 1 KB）。
2. 20 步 harness：activity 总数 ≤ 90（基线 129），事件数 ≤ 340（基线约 450）。
3. 同一批输入下 turn 的对外行为不变：懒唤醒时机、每步落库顺序、memory pass、打断、锁释放。
4. 老会话（旧 `tool_call` 形状）仍能读出正确历史。
5. `go test ./... -count=1` 全绿；真 e2e 跑通。

## 设计

### 1. 历史边界：activity 自读 PG

```
TurnWorkflow（只持控制状态：step / sandboxID / lastSeq）
  └─ CallLLM{SessionID}
        ├─ ListMessages(sessionID, after=0)
        ├─ projectHistory → []agent.Message
        ├─ systemPrompt(sessionID)（失败降级为无记忆注入，warn）
        └─ Agent.Step(ctx, history, tools)
```

- activity 入参是**指针**（SessionID），不是快照；重试时重读最新落库状态（读幂等）。
- workflow 不持有、不传递历史；`LoadHistory` / `BuildSystemPrompt` 两个 activity 删除。
- 否决方案：独立 `LoadHistory` 返回 `HistoryRef{SessionID, UptoSeq}`（多一次往返 + 3 个事件，且 ref 本质还是 ID）；workflow 内维护 `[]agent.Message`（文本随决策进 history，正是 4.29 MB 的来源，且有确定性风险）。

### 2. 消息形状：一个 assistant 回合一行

```
tool_call    {"text":"我来跑","calls":[{"id":"c1","name":"shell","input":"{...}"}]}
              // text 为空时省略该 key
tool_result  {"tool_call_id":"c1","name":"shell","output":"{...}"}   // 不变
assistant    {"text":"最终回答"}                                    // 不变
旧行         {"id":"c1","name":"shell","input":"{...}"}              // 兼容：单调用 + 空 text
```

- 投影：`user→user`、`tool_call→assistant(text + ToolCalls)`、`tool_result→tool`、`assistant→assistant`。
- 为什么必须一步一行：旧实现一个调用一行，同一回合的多个调用被拆成多条 assistant 消息，回灌 provider 时 `tool_use` / `tool_result` 的配对关系丢失（实测：新形状被旧解析器读成"一个空调用"）。
- 编解码只在 `internal/store/pg/content.go`（`ToolCall` / `ToolStep` / `MarshalToolStep` / `ParseToolStep`），`pg` 包不 import `agent`。
- SPA：`content.calls` 渲染工具名，`content.text` 作前缀；保留 `content?.text ?? JSON.stringify` 兜底。

### 3. Activity 清单与重试（Temporal 最佳实践）

| activity | 入参 | 策略 | 说明 |
|---|---|---|---|
| `CallLLM` | `{SessionID}` | `MaximumAttempts: 5`、`ScheduleToClose: 5min`、`HeartbeatTimeout: 30s` | 心跳 10s 一次；官方：Activities must heartbeat to receive cancellations |
| `ExecTool` | 不变 | `MaximumAttempts: 1` | shell 有副作用且 agentd 无幂等键，重试=重跑命令 |
| `WriteMessage` | 加 `MsgID` | 同上（5 次 / 5min） | 幂等键 = `uuid.NewSHA1(uuid.NameSpaceOID, "<turnID>#<写序号>")`；纯函数，不用 `SideEffect`（不多记 Marker 事件），重放结果恒定 |
| `ExecMemoryTool` | 不变 | `MaximumAttempts: 1` | `memory_save` 走 `ApplyMemoryOps` 直接 add，**无幂等键**；失败以 `exit_code=-1` 的 tool_result 返回，模型可自行重试 |
| `DrainQueue` / `EnsureAwake` / `ReleaseTurn` / `StartMemoryPass` | 不变 | 5 次 / 5min | 3 次封顶在 `MemoryPassWorkflow` 内部（不变），不在这个 activity 上 |
| 删除 | `LoadHistory` `BuildSystemPrompt` `PublishChanged` | — | 信号折进 `WriteMessage`（失败只 warn） |

`ScheduleToCloseTimeout` 必须 < 会话锁 TTL（600s），否则锁会在 turn 结束前过期。

### 4. 失败与取消

- 变更信号、记忆注入：可丢/可降级，只 warn，不进关键路径。
- 工具失败：仍以 `exit_code=-1` 的 tool_result 返回给模型（不是 activity error）。
- 打断：`CancelWorkflow` 经心跳穿透到 activity 的 `ctx`，provider 的 HTTP 请求被取消。
- 部署：activity 入参签名变更（`CallLLM` 改为 `CallLLMInput`、`WriteMessage` 新增 `MsgID`），**部署前确认没有 Running 的 `TurnWorkflow`**（单机单用户，等待期秒级）。不做 `GetVersion` 兼容层（YAGNI）。

### 5. 改动清单

| 文件 | 改动 |
|---|---|
| `internal/store/pg/content.go` | `ToolCall`/`ToolStep`/`wire` 结构；`MarshalToolStep`/`ParseToolStep`（兼容旧形状）；删 `MarshalToolCall`/`ParseToolCall` |
| `internal/store/pg/content_test.go` | 新形状 / 旧形状 / 非法 / `{}` |
| `internal/workflow/history.go` | `KindToolCall` 分支改用 `ParseToolStep`，一条 assistant |
| `internal/workflow/history_test.go`（新增） | 多调用不拆散、旧形状可读 |
| `internal/workflow/activities.go` | `loadHistory` 私有化；`CallLLMInput{SessionID}`；`decide` 收 tools；`WriteMessage` 幂等键 + 信号；`heartbeat` 助手；删三个 activity |
| `internal/workflow/prompt.go` | `BuildSystemPrompt` → 私有 `systemPrompt(ctx, sid)` |
| `internal/workflow/turn.go` | 删 workflow 内 history；`CallLLM{SessionID}`；一步一行落库；写闭包带 `msgIDFor`；三套 `ActivityOptions` |
| `internal/workflow/turn_test.go` / `activities_test.go` / `memory_test.go` / `loadhistory_test.go` | 跟随签名与形状 |
| `internal/workflow/options_test.go`（新增） | 策略断言（次数、S2C、心跳） |
| `internal/workflow/cost_test.go`（新增） | 成本回归：脚本化 mock 跑 20 步，断言 activity ≤ 90、`CallLLM` 累计入参 < 5KB |
| `web/src/App.jsx` | 渲染 `content.calls` |
| `CONTEXT.md` | 已更新：`msg_id` 归属、PG 是唯一 prompt 源 |

### 6. 实施顺序

1. `content.go` + 测试（旧形状兼容先绿）。
2. `history.go` 投影 + 测试（红→绿）。
3. `activities.go`：`CallLLM` 自读、心跳、写入幂等；类型与单测跟随。
4. `turn.go`：loop 与 ActivityOptions；workflow 测试。
5. SPA 渲染；`cost_test.go` 指标断言；真 e2e（隔离队列）。

提交拆分：`feat/turn-history-cost` 上两个 commit——先 cost（形状 + 自读 PG + 折叠信号），
再策略（幂等键 + 重试上限 + 心跳），便于逐个 review / 回滚。

## Tech Stack

Go 1.27.3 · `go.temporal.io/sdk` v1.50.0 · `github.com/google/uuid` v1.6.0 · `jackc/pgx/v5` v5.11.0 · `redis/go-redis/v9` v9.23.0 · `stretchr/testify` v1.12.1 · `charm.land/fantasy` v0.45.5 · React 19 + Vite 6（`web/`）

## Commands

```bash
# 构建 / 静态检查
go build ./...
gofmt -l internal cmd && go vet ./...

# 全量测试（必须指向 152 的测试库，绝不指业务库）
# 账号/密码与 .env 的 DATABASE_URL 相同，仅库名不同：ys_sandbox_test{,_api,_wf,_hs,_vd}。
# 五个库必须分开——go test 包间并行，共库 ResetSchema 必撞。
export YS_TEST_PG='postgres://<user>:<pwd>@152.136.212.59:15432/ys_sandbox_test'
export YS_TEST_PG_API='postgres://<user>:<pwd>@152.136.212.59:15432/ys_sandbox_test_api'
export YS_TEST_PG_WF='postgres://<user>:<pwd>@152.136.212.59:15432/ys_sandbox_test_wf'
export YS_TEST_PG_HS='postgres://<user>:<pwd>@152.136.212.59:15432/ys_sandbox_test_hs'
export YS_TEST_PG_VD='postgres://<user>:<pwd>@152.136.212.59:15432/ys_sandbox_test_vd'
export YS_TEST_REDIS=152.136.212.59:16379 REDIS_PASSWORD='<见 .env>'
go test ./... -count=1

# 单包 / 单测试
go test ./internal/workflow -run TestProjectHistory -count=1 -v

# SPA
cd web && npm run build

# 本地进程（依赖都在 152）
go run ./cmd/ys-server
go run ./cmd/ys-worker

# 真 e2e：临时程序注册 TurnWorkflow 到隔离队列 ys-turns-smoke，
# 用真 PG/Redis + 真模型跑一个 turn，随后删除临时程序。
```

## Project Structure

```
cmd/                    进程入口（ys-server / ys-worker / agentd / vmhostd）
internal/workflow/      TurnWorkflow、activities、投影、prompt、memory pass
internal/store/pg/      事实源；content.go 是消息 JSON 形状的唯一归属
internal/store/redis/   协调态（队列/锁/信号）
internal/agent/         fantasy 适配层（业务代码不 import fantasy）
web/src/                SPA；web/dist 被 go:embed 内嵌
docs/ys-powers/intent/  确认的意图
docs/ys-powers/specs/   本 spec
```

## Code Style

```go
// 中文注释；注释只写"为什么"，不写"做了什么"。
// 写消息带确定性幂等键：activity 至少一次，重试/崩溃恢复不能写重复行。
writeNo := 0
write := func(in WriteMessageInput) (int64, error) {
	writeNo++
	in.MsgID = msgIDFor(turnID, writeNo) // SHA1(NameSpaceOID, "<turnID>#<n>")，纯函数
	var seq int64
	err := workflow.ExecuteActivity(ctx, "WriteMessage", in).Get(ctx, &seq)
	return seq, err
}
```

- 无 emoji；标识符英文、注释中文。
- workflow 代码禁止 I/O、随机、`time.Now`（本设计用 name-based UUID 而非 `SideEffect`）。
- 错误用 `fmt.Errorf("...: %w", err)` 包装；可降级路径只 `warn`。

## Testing Strategy

| 层 | 位置 | 覆盖 |
|---|---|---|
| 单元（workflow） | `internal/workflow/*_test.go` + `testsuite.WorkflowTestEnvironment` | loop 分支、懒唤醒、每步写入顺序、msg_id 互异、策略断言 |
| 单元（活动） | 同上，`&Activities{Agent: fakeAgent}` | tool 门控、参数校验、心跳 no-op（非 activity 上下文） |
| 集成（真 PG） | `internal/workflow/loadhistory_test.go`、`activities_test.go` | 投影往返、旧形状、幂等重试、system prompt 注入与降级 |
| e2e | 临时 `cmd/` + 隔离队列 | 真 Temporal + 真 PG/Redis + 真模型，一步工具回合 |
| 成本回归 | `internal/workflow/cost_test.go` | 20 步下 activity 数、`CallLLM` 入参字节（mock 掉 activity，量的是 workflow 传给它的 payload） |

纪律：先写失败测试再实现（红→绿），红要能复现旧缺陷；不写只验证接线/默认值的空壳测试。

覆盖率：**不设门槛**（项目无覆盖率基建）；判断标准是"每条 Success Criteria 都有对应的行为断言"。
手工验证只剩底层 e2e（临时程序，不入库）；其余全部自动化。

## Boundaries

- **Always**：先红后绿；workflow 保持确定性；activity 入参只放标识符；提交前跑全量测试与 `gofmt`。
- **Ask first**：改 `messages` 表结构；新增依赖；改 task queue 名或 workflow/activity 注册名；调整 `web/dist` 的入库策略。
- **Never**：把对话全文当 activity 入参；在 workflow 内直接访问 PG/Redis；对无幂等键的副作用设置 >1 次重试；删除旧形状解析（线上有老数据）。

## Success Criteria

1. `go test ./... -count=1` 全绿（五库）；`go build ./...`、`gofmt -l` 干净。
2. `TestTurnCost`：20 步下 activity ≤ 90、`CallLLM` 累计入参 < 5 KB。
3. 真 e2e：`tool_call` / `tool_result` / `assistant` 三行均带非空唯一 `msg_id`；用同一 `msg_id` 重放 `WriteMessage` 行数不变。
4. `TestProjectHistory_MultiCallStep` 在改动前代码上失败、改动后通过；`TestProjectHistory_LegacySingleCall` 两边都通过。
5. `options_test` 断言：`ExecTool` `MaximumAttempts==1`；可重试 activity ≤5 次且 `ScheduleToClose < 600s`；`CallLLM` `HeartbeatTimeout > 0` 且 `< StartToClose`。

## Open Questions

1. **`web/dist` 入库策略**：`index.html` 被跟踪、`assets/*` 被 gitignore；每次 `npm run build` 都会改 `index.html` 并指向未入库的 hash。是否改为完全不入库（部署前构建）？
2. **部署窗口**：需确认发布时没有 Running 的 `TurnWorkflow`（不做签名兼容层）。
