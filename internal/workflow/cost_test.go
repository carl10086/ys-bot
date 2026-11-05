package workflow

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/testsuite"

	"ys-bot/internal/agent"
)

// 成本回归（spec Success Criteria 2）。
// 改前：20 步 turn = 129 个 activity，CallLLM 入参累计 4.29 MB
// （每次调用都把整份对话写进 ActivityTaskScheduled）。
// 改后：CallLLM 只收 SessionID，历史由 activity 自读；上限见下方断言。
func TestTurnCost(t *testing.T) {
	const steps = 20
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{})

	var activities, llmBytes int
	tc := agent.ToolCall{ID: "c1", Name: "memory_search", Input: `{"query":"偏好"}`}
	countLLM := func(a mock.Arguments) {
		activities++
		b, _ := json.Marshal(a.Get(1))
		llmBytes += len(b)
	}
	count := func(mock.Arguments) { activities++ }

	env.OnActivity("DrainQueue", mock.Anything, "s1").Run(count).Return([]int64{1}, nil)
	env.OnActivity("CallLLM", mock.Anything, mock.Anything).Run(countLLM).
		Return(agent.StepResult{Text: "我查一下", ToolCalls: []agent.ToolCall{tc}}, nil).Times(steps)
	env.OnActivity("CallLLM", mock.Anything, mock.Anything).Run(countLLM).
		Return(agent.StepResult{Text: "完成"}, nil).Once()
	env.OnActivity("ExecMemoryTool", mock.Anything, mock.Anything).Run(count).Return(`{"hits":[]}`, nil)
	env.OnActivity("WriteMessage", mock.Anything, mock.Anything).Run(count).Return(int64(1), nil)
	env.OnActivity("StartMemoryPass", mock.Anything, "s1", mock.Anything, mock.Anything).Run(count).Return(nil)
	env.OnActivity("ReleaseTurn", mock.Anything, "s1", mock.Anything).Run(count).Return(nil)

	env.ExecuteWorkflow(TurnWorkflow, TurnInput{SessionID: "s1"})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	env.AssertExpectations(t)

	t.Logf("20 步：activities=%d  CallLLM 入参累计=%d B", activities, llmBytes)
	if activities > 90 {
		t.Fatalf("activity 数 %d > 90（基线 129）", activities)
	}
	if llmBytes > 5*1024 {
		t.Fatalf("CallLLM 入参累计 %d B > 5KB（基线 4.29 MB）", llmBytes)
	}
}
