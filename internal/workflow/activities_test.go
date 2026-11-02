package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"ys-sandbox/internal/agent"
)

// fakeAgent / fakeDriver：只存在于测试的替身，不进非测试代码。
type fakeAgent struct{ gotTools []agent.ToolDef }

func (f *fakeAgent) Step(_ context.Context, _ []agent.Message, tools []agent.ToolDef) (agent.StepResult, error) {
	f.gotTools = tools
	return agent.StepResult{Text: "ok"}, nil
}

type fakeDriver struct{}

func (fakeDriver) EnsureAwake(context.Context, string) (uuid.UUID, error) { return uuid.Nil, nil }
func (fakeDriver) Exec(context.Context, uuid.UUID, string, uint32) (int32, string, string, error) {
	return 0, "", "", nil
}
func (fakeDriver) SleepVM(context.Context, uuid.UUID) error { return nil }

// 懒唤醒门控：Driver=nil（无 VM 环境）时 CallLLM 不给 VM 工具；
// M5 起 memory_* 服务端工具始终可用（纯对话也要能记，spec Q6）。
func TestCallLLM_NoDriverNoTools(t *testing.T) {
	fa := &fakeAgent{}
	if _, err := (&Activities{Agent: fa}).CallLLM(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	for _, tool := range fa.gotTools {
		if tool.Name == "shell" {
			t.Fatalf("Driver=nil 时不应给 shell，got %v", fa.gotTools)
		}
	}
	if len(fa.gotTools) != len(memoryToolDefs) {
		t.Fatalf("Driver=nil 时应只有 memory 工具，got %v", fa.gotTools)
	}
}

// 有 Driver 时追加 shell 工具（memory 工具仍在）。
func TestCallLLM_WithDriverGivesShell(t *testing.T) {
	fa := &fakeAgent{}
	if _, err := (&Activities{Agent: fa, Driver: fakeDriver{}}).CallLLM(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(fa.gotTools) != len(memoryToolDefs)+1 || fa.gotTools[len(fa.gotTools)-1].Name != "shell" {
		t.Fatalf("want memory 三工具 + shell，got %v", fa.gotTools)
	}
}

// ExecTool 入参校验：未知工具名 / 非法 input 都必须报错（不碰 Driver）。
// M5 e2e 修订：工具参数/用法错误 = 模型可见的 exit_code=-1 输出（模型据此纠错），
// 不是 activity error——后者被 Temporal 无限重试会卡死 turn。
func TestExecTool_Validation(t *testing.T) {
	a := &Activities{}
	for _, in := range []ExecToolInput{
		{SandboxID: uuid.NewString(), Name: "nope", Input: `{}`},          // 未知工具
		{SandboxID: uuid.NewString(), Name: "shell", Input: `not json`},   // 非法 input
		{SandboxID: uuid.NewString(), Name: "shell", Input: `{"cmd":""}`}, // 空 cmd
	} {
		out, err := a.ExecTool(context.Background(), in)
		if err != nil {
			t.Fatalf("%v 不应是 activity error: %v", in, err)
		}
		if !strings.Contains(out, `"exit_code":-1`) {
			t.Fatalf("%v 应返回 exit_code=-1: %s", in, out)
		}
	}
}
