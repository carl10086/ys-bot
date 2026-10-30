package workflow

import (
	"context"
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

// 懒唤醒门控：Driver=nil（无 VM 环境）时 CallLLM 不得把工具给模型。
func TestCallLLM_NoDriverNoTools(t *testing.T) {
	fa := &fakeAgent{}
	if _, err := (&Activities{Agent: fa}).CallLLM(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(fa.gotTools) != 0 {
		t.Fatalf("Driver=nil 时 tools 应为空，got %v", fa.gotTools)
	}
}

// 有 Driver 时给 shell 工具（M2 唯一工具）。
func TestCallLLM_WithDriverGivesShell(t *testing.T) {
	fa := &fakeAgent{}
	if _, err := (&Activities{Agent: fa, Driver: fakeDriver{}}).CallLLM(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(fa.gotTools) != 1 || fa.gotTools[0].Name != "shell" {
		t.Fatalf("want [shell]，got %v", fa.gotTools)
	}
}

// ExecTool 入参校验：未知工具名 / 非法 input 都必须报错（不碰 Driver）。
func TestExecTool_Validation(t *testing.T) {
	a := &Activities{}
	if _, err := a.ExecTool(context.Background(), ExecToolInput{
		SandboxID: uuid.NewString(), Name: "nope", Input: `{}`,
	}); err == nil {
		t.Fatal("unknown tool 必须报错")
	}
	if _, err := a.ExecTool(context.Background(), ExecToolInput{
		SandboxID: uuid.NewString(), Name: "shell", Input: `not json`,
	}); err == nil {
		t.Fatal("非法 input 必须报错")
	}
	if _, err := a.ExecTool(context.Background(), ExecToolInput{
		SandboxID: uuid.NewString(), Name: "shell", Input: `{"cmd":""}`,
	}); err == nil {
		t.Fatal("空 cmd 必须报错")
	}
}
