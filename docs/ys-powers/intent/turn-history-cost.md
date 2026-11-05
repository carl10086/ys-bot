# TurnWorkflow 的 event history 成本 — 意图文档

> 产出：`/clarify-intent`（2026-11-05），经访谈确认，用户明确 yes。
> 前置：`docs/ys-powers/intent/ys-bot.md`。
> 追溯：本意图在实现完成后才补录（流程事故）；代码改动在补录前未提交，
> 红绿证据见文末"TDD 证据"。

## 确认的意图

```
Outcome:      TurnWorkflow 的 event history 只记录"不可重建的决策"
              （StepResult、tool 输出、sandboxID、batch），不再搬运整份对话：
              CallLLM 入参 4.29MB/turn → ~0；可丢的变更信号不再单独占 activity 往返
              （41 次/turn → 0）。
User:         你自己（唯一用户）——长会话不撞 Temporal 的 2MB payload / 4MB
              transaction 硬线，turn 的 replay 成本与会话长度无关。
Why now:      实测 20 步 turn 累计入参 4.29MB、129 个 activity；会话继续变长就会撞线，
              撞线后的报错（payload size exceeded）不可读且 turn 直接卡死。
Success:      同一批输入的 turn 产出不变；go test ./... 全绿；真 e2e 跑通；
              单个 turn 的 CallLLM 入参 < 1KB；老会话（旧 tool_call 形状）仍可读。
Constraint:   （1）不改 agent loop 的可观察语义：懒唤醒、每步落库可见、memory pass、
              打断、每步 activity 的重试边界；
              （2）符合 Temporal 官方最佳实践（见下节核对表）。
Out of scope: 合并 ExecTool 与写操作（需先给 agentd 加幂等键）、DrainQueue 幂等修复、
              history 窗口/压缩策略、跨节点调度。
```

## 关键判定

1. **activity 入参会进 event history**（`ActivityTaskScheduled`），所以入参只能是
   "指针"（SessionID/seq），不能是数据快照（历史、sysprompt）。
2. **按"重试单元"切 activity，不按函数切**：纯读不独立成 activity（读失败重试一次
   整个 LLM 调用即可）；不可重复副作用必须独占 activity。
3. **可丢信号不占关键路径**：Redis 变更通知丢了会由下次刷新补，不值得一次
   task queue 往返 + 3 个事件。
4. **PG 是唯一 prompt 源**：tool_call 一步一行（`{"text","calls":[...]}`），
   投影时一条 assistant + 若干 tool_result，不再把一个 assistant 的多个调用拆散。

## Temporal 最佳实践核对（本次逐项落实）

| # | 实践 | 现状 → 处理 |
|---|---|---|
| 1 | activity 入参尽量小，传标识符不传大对象 | 已落实：`CallLLMInput{SessionID}` |
| 2 | activity 至少一次，重试 → 副作用必须幂等 | 已落实：`WriteMessage` 带确定性 `msg_id`（`uuid.NewSHA1(turnID#n)`），重试不重复写行 |
| 3 | 默认 RetryPolicy 是无限重试，必须显式收敛 | 已落实：读/幂等写 5 次 + `ScheduleToCloseTimeout` 5min（< 会话锁 TTL 10min） |
| 4 | 非幂等副作用不应盲目重试 | 已落实：`ExecTool`（shell 无幂等键）`MaximumAttempts: 1` |
| 5 | 长阻塞 activity 必须心跳，否则收不到取消 | 已落实：`CallLLM` 心跳 10s / `HeartbeatTimeout` 30s，取消可穿透到 HTTP |
| 6 | workflow 代码确定性：无 I/O、无随机 | 已保持：UUID 用 name-based SHA1（纯函数），不用 `SideEffect`（会多记 Marker 事件） |

## 已知未落实（明确记录，不在本次范围）

- `DrainQueue` = `LRANGE + DEL` 不幂等：activity 成功但 worker 在 ACK 前崩溃 → 重试拿到
  空批 → `turn.go` 直接 return，用户消息静默丢失。修法是 `TurnInput` 带触发 seq，
  drain 退化为合并优化——单独立项。
- activity 签名变更（`CallLLM: []agent.Message → CallLLMInput`）对在飞 turn 不兼容：
  部署前需确认没有 Running 的 `TurnWorkflow`（个人单机系统，等待期以秒计）。

## TDD 证据（2026-11-05）

1. **先红**：新增 4 个测试后直接跑，编译期红（`MsgID` 字段、`aoExec/aoRetry/aoLLM` 未定义），
   证明测试描述的是尚不存在的行为。
2. **行为级红**：在 `git worktree` 的 HEAD（改动前）上单独跑
   `TestProjectHistory_MultiCallStep` → `FAIL: assistant 回合 = {Role:assistant Content: ToolCalls:[{ID: Name: Input:}]}`
   （旧投影把新形状的 tool_call 解析成空调用）。改后同测试绿。
3. **后绿**：`go test ./internal/workflow -count=1` 全绿；
   `go test ./... -count=1` 全绿（152 的五个测试库）。
4. **真 e2e**（隔离队列 `ys-turns-smoke`，真 Temporal + 真 PG/Redis + 真 Kimi）：
   tool_call/tool_result/assistant 三行都带 `msg_id`；用同一 `msg_id` 回放
   `WriteMessage`，行数保持 4；event history 65 events / 9.7 KB。
