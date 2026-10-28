// Package workflow Temporal workflow 与 activities。
// TurnWorkflow = 讲座 15 "The turn" 的实现：一批消息进来，跑一轮，每步落库，跑完即终。
package workflow

import (
	"fmt"
	"time"

	"go.temporal.io/sdk/workflow"

	"ys-sandbox/internal/agent"
)

// TaskQueue ys-worker 监听的 Temporal task queue。
const TaskQueue = "ys-turns"

type TurnInput struct {
	SessionID string
}

type WriteMessageInput struct {
	SessionID string
	Kind      string // assistant（M1 只会写这种）
	Text      string // 门面内包成 {"text": ...} jsonb
	TurnID    string
}

// TurnWorkflow 一个 turn 的完整生命周期。
// 锁的释放用 disconnected context：被打断（CancelWorkflow）时也要放锁。
func TurnWorkflow(ctx workflow.Context, in TurnInput) error {
	ao := workflow.ActivityOptions{
		StartToCloseTimeout: 2 * time.Minute,
		// LLM 调用可能抖动，activity 级重试交给 Temporal 默认策略
	}
	ctx = workflow.WithActivityOptions(ctx, ao)
	logger := workflow.GetLogger(ctx)

	defer func() {
		dctx, _ := workflow.NewDisconnectedContext(ctx)
		_ = workflow.ExecuteActivity(dctx, "ReleaseTurn", in.SessionID).Get(dctx, nil)
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

	var reply string
	if err := workflow.ExecuteActivity(ctx, "CallLLM", history).Get(ctx, &reply); err != nil {
		return fmt.Errorf("call llm: %w", err)
	}

	turnID := workflow.GetInfo(ctx).WorkflowExecution.ID
	var seq int64
	err := workflow.ExecuteActivity(ctx, "WriteMessage", WriteMessageInput{
		SessionID: in.SessionID,
		Kind:      "assistant",
		Text:      reply,
		TurnID:    turnID,
	}).Get(ctx, &seq)
	if err != nil {
		return fmt.Errorf("write message: %w", err)
	}

	if err := workflow.ExecuteActivity(ctx, "PublishChanged", in.SessionID).Get(ctx, nil); err != nil {
		return fmt.Errorf("publish changed: %w", err)
	}
	logger.Info("turn done", "batch", len(batch), "replySeq", seq)
	return nil
}
