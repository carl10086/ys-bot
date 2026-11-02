package workflow

import (
	"encoding/json"
	"errors"
	"strings"
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
	env.OnActivity("BuildSystemPrompt", mock.Anything, "s1").Return("", nil)
	env.OnActivity("CallLLM", mock.Anything, mock.Anything).Return(agent.StepResult{Text: "你好，我是助手"}, nil)
	env.OnActivity("WriteMessage", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { written = args.Get(1).(WriteMessageInput) }).
		Return(int64(3), nil)
	env.OnActivity("PublishChanged", mock.Anything, "s1").Return(nil)
	env.OnActivity("ReleaseTurn", mock.Anything, "s1", mock.Anything).Return(nil)

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
	env.OnActivity("ReleaseTurn", mock.Anything, "s1", mock.Anything).Return(nil)
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
	env.OnActivity("BuildSystemPrompt", mock.Anything, "s1").Return("", nil)
	env.OnActivity("CallLLM", mock.Anything, mock.Anything).Return("", errors.New("llm down"))
	env.OnActivity("ReleaseTurn", mock.Anything, "s1", mock.Anything).Return(nil)

	env.ExecuteWorkflow(TurnWorkflow, TurnInput{SessionID: "s1"})
	if err := env.GetWorkflowError(); err == nil {
		t.Fatal("want workflow error from LLM failure")
	}
	env.AssertExpectations(t) // ReleaseTurn 必须被调到
}

// M4/T5：ExecTool 失败时 tool_result 的 content 必须是合法 JSON
// （修复前用 Sprintf 拼接，stderr 含引号会生成非法 JSON）。
func TestTurnWorkflow_ExecErrorProducesValidJSON(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{})

	tc := agent.ToolCall{ID: "c1", Name: "shell", Input: `{"cmd":"ls"}`}
	env.OnActivity("DrainQueue", mock.Anything, "s1").Return([]int64{1}, nil)
	env.OnActivity("LoadHistory", mock.Anything, "s1").Return([]agent.Message{{Role: "user", Content: "hi"}}, nil)
	env.OnActivity("BuildSystemPrompt", mock.Anything, "s1").Return("", nil)
	env.OnActivity("CallLLM", mock.Anything, mock.Anything).
		Return(agent.StepResult{ToolCalls: []agent.ToolCall{tc}}, nil).Once()
	env.OnActivity("CallLLM", mock.Anything, mock.Anything).
		Return(agent.StepResult{Text: "命令失败了"}, nil).Once()
	env.OnActivity("EnsureAwake", mock.Anything, "s1").Return("sb-1", nil)
	// 测试环境默认带 activity 重试：失败会被多次调用，不能 Once()
	env.OnActivity("ExecTool", mock.Anything, mock.Anything).
		Return("", errors.New(`dial "10.200.0.2" refused`))
	var toolResult json.RawMessage
	env.OnActivity("WriteMessage", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			in := args.Get(1).(WriteMessageInput)
			if in.Kind == "tool_result" {
				toolResult = in.Content
			}
		}).Return(int64(1), nil)
	env.OnActivity("PublishChanged", mock.Anything, "s1").Return(nil)
	env.OnActivity("ReleaseTurn", mock.Anything, "s1", mock.Anything).Return(nil)

	env.ExecuteWorkflow(TurnWorkflow, TurnInput{SessionID: "s1"})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	env.AssertExpectations(t)

	if len(toolResult) == 0 {
		t.Fatal("tool_result 未落库")
	}
	var parsed struct {
		ToolCallID string `json:"tool_call_id"`
		Output     string `json:"output"`
	}
	if err := json.Unmarshal(toolResult, &parsed); err != nil {
		t.Fatalf("tool_result content 非法 JSON: %v\nraw: %s", err, toolResult)
	}
	var out struct {
		ExitCode int    `json:"exit_code"`
		Stderr   string `json:"stderr"`
	}
	if err := json.Unmarshal([]byte(parsed.Output), &out); err != nil {
		t.Fatalf("tool_result output 非法 JSON: %v\nraw: %s", err, parsed.Output)
	}
	// activity 错误带包装前缀，断言包含原始文本即可（核心是 JSON 合法 + exit_code=-1）
	if out.ExitCode != -1 || !strings.Contains(out.Stderr, `dial "10.200.0.2" refused`) {
		t.Fatalf("out = %+v", out)
	}
}

// 模型先给 tool_calls → 懒唤醒一次 → 逐个执行（tool_call/tool_result 各落一条）
// → 第二轮模型回文本收尾。

// M2 工具 loop（spec §4 / plan T4）：
// 模型先给 tool_calls → 懒唤醒一次 → 逐个执行（tool_call/tool_result 各落一条）
// → 第二轮模型回文本收尾。
func TestTurnWorkflow_ToolLoop(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{})

	var kinds []string
	tc := agent.ToolCall{ID: "call_1", Name: "shell", Input: `{"cmd":"ls /etc | head -5"}`}
	env.OnActivity("DrainQueue", mock.Anything, "s1").Return([]int64{1}, nil)
	env.OnActivity("LoadHistory", mock.Anything, "s1").Return([]agent.Message{{Role: "user", Content: "看看 /etc"}}, nil)
	env.OnActivity("BuildSystemPrompt", mock.Anything, "s1").Return("", nil)
	env.OnActivity("CallLLM", mock.Anything, mock.Anything).
		Return(agent.StepResult{ToolCalls: []agent.ToolCall{tc}}, nil).Once()
	env.OnActivity("CallLLM", mock.Anything, mock.Anything).
		Return(agent.StepResult{Text: "/etc 下有 hosts、passwd 等"}, nil).Once()
	env.OnActivity("EnsureAwake", mock.Anything, "s1").Return("sb-1", nil).Once()
	env.OnActivity("ExecTool", mock.Anything, mock.Anything).
		Return(`{"exit_code":0,"stdout":"hosts\npasswd\n","stderr":""}`, nil).Once()
	env.OnActivity("WriteMessage", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { kinds = append(kinds, args.Get(1).(WriteMessageInput).Kind) }).
		Return(int64(1), nil)
	env.OnActivity("PublishChanged", mock.Anything, "s1").Return(nil)
	env.OnActivity("ReleaseTurn", mock.Anything, "s1", mock.Anything).Return(nil)

	env.ExecuteWorkflow(TurnWorkflow, TurnInput{SessionID: "s1"})

	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	env.AssertExpectations(t) // EnsureAwake 恰好一次（懒唤醒）等 Once() 断言
	want := []string{"tool_call", "tool_result", "assistant"}
	if len(kinds) != len(want) {
		t.Fatalf("kinds = %v", kinds)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", kinds, want)
		}
	}
}

// 懒唤醒红线：纯对话 turn 不得触发 EnsureAwake。
// （未注册 EnsureAwake 的 mock —— 一旦被调用 testify 直接报 unexpected call。）
func TestTurnWorkflow_PlainChatNeverWakesVM(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{})

	env.OnActivity("DrainQueue", mock.Anything, "s1").Return([]int64{1}, nil)
	env.OnActivity("LoadHistory", mock.Anything, "s1").Return([]agent.Message{{Role: "user", Content: "hi"}}, nil)
	env.OnActivity("CallLLM", mock.Anything, mock.Anything).Return(agent.StepResult{Text: "hello"}, nil)
	env.OnActivity("BuildSystemPrompt", mock.Anything, "s1").Return("", nil)
	env.OnActivity("WriteMessage", mock.Anything, mock.Anything).Return(int64(2), nil)
	env.OnActivity("PublishChanged", mock.Anything, "s1").Return(nil)
	env.OnActivity("ReleaseTurn", mock.Anything, "s1", mock.Anything).Return(nil)

	env.ExecuteWorkflow(TurnWorkflow, TurnInput{SessionID: "s1"})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	env.AssertExpectations(t)
}

// 失控防护：模型每轮都要工具 → 达上限后收尾，不写第 21 次 tool_call。
func TestTurnWorkflow_ToolLoopCap(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{})

	toolCalls := 0
	tc := agent.ToolCall{ID: "c", Name: "shell", Input: `{"cmd":"true"}`}
	env.OnActivity("DrainQueue", mock.Anything, "s1").Return([]int64{1}, nil)
	env.OnActivity("LoadHistory", mock.Anything, "s1").Return([]agent.Message{{Role: "user", Content: "loop"}}, nil)
	env.OnActivity("BuildSystemPrompt", mock.Anything, "s1").Return("", nil)
	env.OnActivity("CallLLM", mock.Anything, mock.Anything).
		Return(agent.StepResult{ToolCalls: []agent.ToolCall{tc}}, nil)
	env.OnActivity("EnsureAwake", mock.Anything, "s1").Return("sb-1", nil).Once()
	env.OnActivity("ExecTool", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { toolCalls++ }).
		Return(`{"exit_code":0,"stdout":"","stderr":""}`, nil)
	env.OnActivity("WriteMessage", mock.Anything, mock.Anything).Return(int64(1), nil)
	env.OnActivity("PublishChanged", mock.Anything, "s1").Return(nil)
	env.OnActivity("ReleaseTurn", mock.Anything, "s1", mock.Anything).Return(nil)

	env.ExecuteWorkflow(TurnWorkflow, TurnInput{SessionID: "s1"})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if toolCalls > maxToolSteps {
		t.Fatalf("toolCalls = %d, 超过上限 %d", toolCalls, maxToolSteps)
	}
}

// M5 注入：turn 启动时 BuildSystemPrompt 的结果必须作为 history[0]（role=system）喂给 CallLLM。
func TestTurnWorkflow_SystemPromptInjected(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{})

	var gotHistory []agent.Message
	env.OnActivity("DrainQueue", mock.Anything, "s1").Return([]int64{1}, nil)
	env.OnActivity("LoadHistory", mock.Anything, "s1").Return([]agent.Message{{Role: "user", Content: "hi"}}, nil)
	env.OnActivity("BuildSystemPrompt", mock.Anything, "s1").Return("BASE\n# 关于用户的记忆\n- 偏好简短", nil)
	env.OnActivity("CallLLM", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { gotHistory = args.Get(1).([]agent.Message) }).
		Return(agent.StepResult{Text: "ok"}, nil)
	env.OnActivity("WriteMessage", mock.Anything, mock.Anything).Return(int64(2), nil)
	env.OnActivity("PublishChanged", mock.Anything, "s1").Return(nil)
	env.OnActivity("ReleaseTurn", mock.Anything, "s1", mock.Anything).Return(nil)

	env.ExecuteWorkflow(TurnWorkflow, TurnInput{SessionID: "s1"})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if len(gotHistory) != 2 || gotHistory[0].Role != "system" {
		t.Fatalf("history[0] 应为 system 注入: %+v", gotHistory)
	}
	if !strings.Contains(gotHistory[0].Content, "偏好简短") {
		t.Fatalf("注入内容不含记忆块: %q", gotHistory[0].Content)
	}
}
