// Package workflow Temporal workflow 与 activities。
// TurnWorkflow = 讲座 15 "The turn" 的实现：一批消息进来，跑一轮，每步落库，跑完即终。
package workflow

import (
	"fmt"
	"time"

	"go.temporal.io/sdk/workflow"

	"ys-sandbox/internal/agent"
	"ys-sandbox/internal/store/pg"
)

// TaskQueue ys-worker 监听的 Temporal task queue。
const TaskQueue = "ys-turns"

type TurnInput struct {
	SessionID string
}

type WriteMessageInput struct {
	SessionID string
	Kind      string // assistant / tool_call / tool_result
	Text      string // Content 为空时门面内包成 {"text": ...} jsonb
	Content   []byte // tool 消息：调用方直接给 jsonb 原文
	TurnID    string
}

// maxToolSteps 单 turn 工具调用步数上限（防 loop 失控）。
const maxToolSteps = 20

// TurnWorkflow 一个 turn 的完整生命周期。
// 锁的释放用 disconnected context：被打断（CancelWorkflow）时也要放锁。
func TurnWorkflow(ctx workflow.Context, in TurnInput) error {
	ao := workflow.ActivityOptions{
		StartToCloseTimeout: 2 * time.Minute,
		// LLM 调用可能抖动，activity 级重试交给 Temporal 默认策略
	}
	ctx = workflow.WithActivityOptions(ctx, ao)
	logger := workflow.GetLogger(ctx)

	turnID := workflow.GetInfo(ctx).WorkflowExecution.ID
	defer func() {
		dctx, _ := workflow.NewDisconnectedContext(ctx)
		// 条件释放：锁可能已被打断者抢走，只放自己的
		_ = workflow.ExecuteActivity(dctx, "ReleaseTurn", in.SessionID, turnID).Get(dctx, nil)
	}()

	var batch []int64
	if err := workflow.ExecuteActivity(ctx, "DrainQueue", in.SessionID).Get(ctx, &batch); err != nil {
		return fmt.Errorf("drain queue: %w", err)
	}
	if len(batch) == 0 {
		logger.Info("empty batch, nothing to do")
		return nil
	}

	var history []agent.Message
	if err := workflow.ExecuteActivity(ctx, "LoadHistory", in.SessionID).Get(ctx, &history); err != nil {
		return fmt.Errorf("load history: %w", err)
	}

	// 工具 loop（spec §4）：模型一次 Step 要么回文本（收尾），要么给一批 tool_calls
	// （首个 tool_call 前懒唤醒 VM → 逐个执行 → 每步落库 + 发信号 → 继续）。
	sandboxID := ""
	var lastSeq int64
	for step := 0; ; step++ {
		var res agent.StepResult
		if err := workflow.ExecuteActivity(ctx, "CallLLM", history).Get(ctx, &res); err != nil {
			return fmt.Errorf("call llm: %w", err)
		}
		if len(res.ToolCalls) == 0 {
			err := workflow.ExecuteActivity(ctx, "WriteMessage", WriteMessageInput{
				SessionID: in.SessionID,
				Kind:      "assistant",
				Text:      res.Text,
				TurnID:    turnID,
			}).Get(ctx, &lastSeq)
			if err != nil {
				return fmt.Errorf("write message: %w", err)
			}
			break
		}
		if step >= maxToolSteps {
			_ = workflow.ExecuteActivity(ctx, "WriteMessage", WriteMessageInput{
				SessionID: in.SessionID,
				Kind:      "assistant",
				Text:      fmt.Sprintf("（工具调用超过单 turn 上限 %d 次，先停在这里）", maxToolSteps),
				TurnID:    turnID,
			}).Get(ctx, &lastSeq)
			break
		}
		if sandboxID == "" { // 懒唤醒：只在首个 tool_call 前
			if err := workflow.ExecuteActivity(ctx, "EnsureAwake", in.SessionID).Get(ctx, &sandboxID); err != nil {
				return fmt.Errorf("ensure awake: %w", err)
			}
		}
		history = append(history, agent.Message{Role: "assistant", Content: res.Text, ToolCalls: res.ToolCalls})
		for _, tc := range res.ToolCalls {
			callContent := pg.MarshalToolCall(tc.ID, tc.Name, tc.Input)
			if err := workflow.ExecuteActivity(ctx, "WriteMessage", WriteMessageInput{
				SessionID: in.SessionID, Kind: "tool_call", Content: callContent, TurnID: turnID,
			}).Get(ctx, nil); err != nil {
				return fmt.Errorf("write tool_call: %w", err)
			}
			_ = workflow.ExecuteActivity(ctx, "PublishChanged", in.SessionID).Get(ctx, nil)

			var out string
			if err := workflow.ExecuteActivity(ctx, "ExecTool", ExecToolInput{
				SandboxID: sandboxID, Name: tc.Name, Input: tc.Input,
			}).Get(ctx, &out); err != nil {
				out = fmt.Sprintf(`{"exit_code":-1,"stderr":"%s"}`, err)
			}
			resultContent := pg.MarshalToolResult(tc.ID, tc.Name, out)
			if err := workflow.ExecuteActivity(ctx, "WriteMessage", WriteMessageInput{
				SessionID: in.SessionID, Kind: "tool_result", Content: resultContent, TurnID: turnID,
			}).Get(ctx, nil); err != nil {
				return fmt.Errorf("write tool_result: %w", err)
			}
			_ = workflow.ExecuteActivity(ctx, "PublishChanged", in.SessionID).Get(ctx, nil)
			history = append(history, agent.Message{Role: "tool", ToolCallID: tc.ID, Name: tc.Name, Content: out})
		}
	}

	if err := workflow.ExecuteActivity(ctx, "PublishChanged", in.SessionID).Get(ctx, nil); err != nil {
		return fmt.Errorf("publish changed: %w", err)
	}
	logger.Info("turn done", "batch", len(batch), "replySeq", lastSeq)
	return nil
}
