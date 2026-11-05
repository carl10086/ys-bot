// Package workflow Temporal workflow 与 activities。
// TurnWorkflow = 讲座 15 "The turn" 的实现：一批消息进来，跑一轮，每步落库，跑完即终。
package workflow

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"ys-bot/internal/agent"
	"ys-bot/internal/store/pg"
)

// TaskQueue ys-worker 监听的 Temporal task queue。
const TaskQueue = "ys-turns"

type TurnInput struct {
	SessionID string
}

type WriteMessageInput struct {
	SessionID string
	MsgID     string // 确定性幂等键（workflow 侧生成；activity 重试不写重复行）
	Kind      string // assistant / tool_call / tool_result
	Text      string // Content 为空时门面内包成 {"text": ...} jsonb
	Content   []byte // tool 消息：调用方直接给 jsonb 原文
	TurnID    string
}

// maxToolSteps 单 turn 工具调用步数上限（防 loop 失控）。
const maxToolSteps = 20

// activity 重试策略是安全约束，不是调参（断言见 options_test.go）：
//   - Temporal 默认策略是无限重试——一个必然失败的 turn 会永久占着会话锁（TTL 600s）；
//   - shell 有副作用且 agentd 没有幂等键，重试 = 重跑命令；
//   - CallLLM 长阻塞，没有心跳就收不到取消请求。
var (
	// aoRetry 读 PG / 幂等写：可安全重试，但次数与总时长都要有界。
	aoRetry = workflow.ActivityOptions{
		StartToCloseTimeout:    2 * time.Minute,
		ScheduleToCloseTimeout: 5 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    2 * time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    30 * time.Second,
			MaximumAttempts:    5,
		},
	}
	// aoExec 不可幂等：失败就交给模型看（exit_code=-1），不重跑命令。
	aoExec = workflow.ActivityOptions{
		StartToCloseTimeout: 2 * time.Minute,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	}
	// aoLLM 必须心跳：Temporal 只把取消投递给有心跳的 activity。
	// 心跳间隔 10s（activity 内），超时 30s 容忍两次丢失。
	aoLLM = workflow.ActivityOptions{
		StartToCloseTimeout: 2 * time.Minute,
		HeartbeatTimeout:    30 * time.Second,
		RetryPolicy:         aoRetry.RetryPolicy,
	}
)

// TurnWorkflow 一个 turn 的完整生命周期。
// 锁的释放用 disconnected context：被打断（CancelWorkflow）时也要放锁。
func TurnWorkflow(ctx workflow.Context, in TurnInput) error {
	ctx = workflow.WithActivityOptions(ctx, aoRetry)
	llmCtx := workflow.WithActivityOptions(ctx, aoLLM)
	execCtx := workflow.WithActivityOptions(ctx, aoExec)
	logger := workflow.GetLogger(ctx)

	turnID := workflow.GetInfo(ctx).WorkflowExecution.ID

	var batch []int64
	if err := workflow.ExecuteActivity(ctx, "DrainQueue", in.SessionID).Get(ctx, &batch); err != nil {
		return fmt.Errorf("drain queue: %w", err)
	}

	defer func() {
		dctx, _ := workflow.NewDisconnectedContext(ctx)
		// 条件释放：锁可能已被打断者抢走，只放自己的
		_ = workflow.ExecuteActivity(dctx, "ReleaseTurn", in.SessionID, turnID).Get(dctx, nil)
		// M5/C1：被打断的 turn 也要提取——用户没等答完就发下一条时，
		// 前一轮的偏好陈述同样值得记住。空批（无新消息）无内容可提取，跳过。
		if len(batch) > 0 {
			if err := workflow.ExecuteActivity(dctx, "StartMemoryPass", in.SessionID, turnID, batch).Get(dctx, nil); err != nil {
				logger.Warn("start memory pass failed", "err", err)
			}
		}
	}()

	if len(batch) == 0 {
		logger.Info("empty batch, nothing to do")
		return nil
	}

	// 工具 loop（spec §4）：模型一次 Step 要么回文本（收尾），要么给一批 tool_calls
	// （首个 tool_call 前懒唤醒 VM → 逐个执行 → 每步落库 → 继续）。
	// 对话历史不在这里维护：CallLLM 每次从 PG 现取（PG 是唯一 prompt 源），
	// workflow 只记控制状态（step / sandboxID / lastSeq）——event history
	// 只随"决策"增长，不随"文本"增长。
	sandboxID := ""
	var lastSeq int64
	// write 落一条消息；变更信号在 WriteMessage 内顺带发出（可丢信号不占独立往返）。
	// 每条写都带确定性幂等键：activity 重试/worker 崩溃后恢复不会写重复行。
	writeNo := 0
	write := func(in WriteMessageInput) (int64, error) {
		writeNo++
		in.MsgID = msgIDFor(turnID, writeNo)
		var seq int64
		err := workflow.ExecuteActivity(ctx, "WriteMessage", in).Get(ctx, &seq)
		return seq, err
	}
	for step := 0; ; step++ {
		var res agent.StepResult
		if err := workflow.ExecuteActivity(llmCtx, "CallLLM", CallLLMInput{SessionID: in.SessionID}).Get(ctx, &res); err != nil {
			return fmt.Errorf("call llm: %w", err)
		}
		if len(res.ToolCalls) == 0 {
			seq, err := write(WriteMessageInput{
				SessionID: in.SessionID,
				Kind:      "assistant",
				Text:      res.Text,
				TurnID:    turnID,
			})
			if err != nil {
				return fmt.Errorf("write message: %w", err)
			}
			lastSeq = seq
			break
		}
		if step >= maxToolSteps {
			seq, _ := write(WriteMessageInput{
				SessionID: in.SessionID,
				Kind:      "assistant",
				Text:      fmt.Sprintf("（工具调用超过单 turn 上限 %d 次，先停在这里）", maxToolSteps),
				TurnID:    turnID,
			})
			lastSeq = seq
			break
		}
		// M5：只有 VM 工具（shell）才懒唤醒；memory_* 走服务端路径，纯记忆对话不碰 VM
		needsVM := false
		for _, tc := range res.ToolCalls {
			if !isMemoryTool(tc.Name) {
				needsVM = true
				break
			}
		}
		if needsVM && sandboxID == "" { // 懒唤醒：只在首个 VM tool_call 前
			if err := workflow.ExecuteActivity(ctx, "EnsureAwake", in.SessionID).Get(ctx, &sandboxID); err != nil {
				return fmt.Errorf("ensure awake: %w", err)
			}
		}
		// 一步一条 assistant 消息：文本 + 全部 tool_calls 同属一个回合，一起落库，
		// projectHistory 才能原样还原成一条 assistant 消息（多调用不会被拆散）。
		if _, err := write(WriteMessageInput{
			SessionID: in.SessionID, Kind: "tool_call",
			Content: pg.MarshalToolStep(pg.ToolStep{Text: res.Text, Calls: pgToolCalls(res.ToolCalls)}),
			TurnID:  turnID,
		}); err != nil {
			return fmt.Errorf("write tool_call: %w", err)
		}
		for _, tc := range res.ToolCalls {
			var out string
			var toolErr error
			if isMemoryTool(tc.Name) {
				// 服务端工具：不经 VM（spec Q6）
				toolErr = workflow.ExecuteActivity(ctx, "ExecMemoryTool", ExecMemoryToolInput{
					SessionID: in.SessionID, Name: tc.Name, Input: tc.Input,
				}).Get(ctx, &out)
			} else {
				toolErr = workflow.ExecuteActivity(execCtx, "ExecTool", ExecToolInput{
					SandboxID: sandboxID, Name: tc.Name, Input: tc.Input,
				}).Get(ctx, &out)
			}
			if toolErr != nil {
				// 工具执行失败也落库（exit_code=-1），让模型看到错误继续决策。
				// 必须 json.Marshal——stderr 含引号/换行时 Sprintf 会拼出非法 JSON。
				b, _ := json.Marshal(map[string]any{"exit_code": -1, "stderr": toolErr.Error()})
				out = string(b)
			}
			if _, err := write(WriteMessageInput{
				SessionID: in.SessionID, Kind: "tool_result",
				Content: pg.MarshalToolResult(tc.ID, tc.Name, out), TurnID: turnID,
			}); err != nil {
				return fmt.Errorf("write tool_result: %w", err)
			}
		}
	}

	logger.Info("turn done", "batch", len(batch), "replySeq", lastSeq)
	return nil
}

// msgIDFor 写消息的确定性幂等键。
// turnID 每次 turn 唯一，写序号在重放时完全一致；name-based UUID 是纯函数，
// 不需要 SideEffect（那会多记一个 Marker 事件），也不会引入随机性。
func msgIDFor(turnID string, n int) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("%s#%d", turnID, n))).String()
}

// pgToolCalls agent 工具调用 → 落库形状（一个 assistant 回合的全部调用写在一条消息里）。
func pgToolCalls(calls []agent.ToolCall) []pg.ToolCall {
	out := make([]pg.ToolCall, 0, len(calls))
	for _, c := range calls {
		out = append(out, pg.ToolCall{ID: c.ID, Name: c.Name, Input: c.Input})
	}
	return out
}
