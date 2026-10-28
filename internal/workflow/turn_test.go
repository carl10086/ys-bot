package workflow

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/testsuite"

	"ys-sandbox/internal/agent"
)

// TurnWorkflow 的期望形状（spec §5 / data-model.md）：
// drain 队列得一批消息 → 载历史 → 调 LLM → 回复写 messages（kind=assistant, 带 turn_id）
// → PUBLISH changed 空信号 → 释放 turn 锁（无论成功/失败）。

func TestTurnWorkflow_HappyPath(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{}) // 只注册名字；真实调用被 OnActivity mock 拦截

	var written WriteMessageInput
	env.OnActivity("DrainQueue", mock.Anything, "s1").Return([]int64{1, 2}, nil)
	env.OnActivity("LoadHistory", mock.Anything, "s1").Return([]agent.Message{
		{Role: "user", Content: "你好"},
	}, nil)
	env.OnActivity("CallLLM", mock.Anything, mock.Anything).Return("你好，我是助手", nil)
	env.OnActivity("WriteMessage", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { written = args.Get(1).(WriteMessageInput) }).
		Return(int64(3), nil)
	env.OnActivity("PublishChanged", mock.Anything, "s1").Return(nil)
	env.OnActivity("ReleaseTurn", mock.Anything, "s1").Return(nil)

	env.ExecuteWorkflow(TurnWorkflow, TurnInput{SessionID: "s1"})

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow not completed")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	env.AssertExpectations(t)

	if written.Kind != "assistant" || written.SessionID != "s1" {
		t.Fatalf("written = %+v", written)
	}
	if written.TurnID == "" {
		t.Fatal("assistant 消息必须带 turn_id（= workflowID）")
	}
	if written.Text != "你好，我是助手" {
		t.Fatalf("content = %s", written.Text)
	}
}

func TestTurnWorkflow_EmptyBatchSkipsLLM(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{}) // 只注册名字；真实调用被 OnActivity mock 拦截

	env.OnActivity("DrainQueue", mock.Anything, "s1").Return([]int64{}, nil)
	env.OnActivity("ReleaseTurn", mock.Anything, "s1").Return(nil)
	// LoadHistory / CallLLM / WriteMessage 未注册 mock，被调用即测试失败

	env.ExecuteWorkflow(TurnWorkflow, TurnInput{SessionID: "s1"})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("empty batch should complete cleanly: %v", err)
	}
	env.AssertExpectations(t)
}

func TestTurnWorkflow_LLMErrorStillReleasesLock(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{}) // 只注册名字；真实调用被 OnActivity mock 拦截

	env.OnActivity("DrainQueue", mock.Anything, "s1").Return([]int64{1}, nil)
	env.OnActivity("LoadHistory", mock.Anything, "s1").Return([]agent.Message{{Role: "user", Content: "hi"}}, nil)
	env.OnActivity("CallLLM", mock.Anything, mock.Anything).Return("", errors.New("llm down"))
	env.OnActivity("ReleaseTurn", mock.Anything, "s1").Return(nil)

	env.ExecuteWorkflow(TurnWorkflow, TurnInput{SessionID: "s1"})
	if err := env.GetWorkflowError(); err == nil {
		t.Fatal("want workflow error from LLM failure")
	}
	env.AssertExpectations(t) // ReleaseTurn 必须被调到
}
