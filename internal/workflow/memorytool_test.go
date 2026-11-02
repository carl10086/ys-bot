package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/testsuite"

	"ys-sandbox/internal/agent"
)

// M5 服务端工具（spec Q6）：memory_* 不经 VM/agentd，在 worker 内直接执行。

// 懒唤醒红线：只用 memory 工具的 turn 绝不能碰 VM（EnsureAwake 未注册，被调即 fail）。
func TestTurnWorkflow_MemoryToolNeverWakesVM(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{})

	tc := agent.ToolCall{ID: "c1", Name: "memory_save", Input: `{"type":"profile","text":"偏好简短"}`}
	env.OnActivity("DrainQueue", mock.Anything, "s1").Return([]int64{1}, nil)
	env.OnActivity("LoadHistory", mock.Anything, "s1").Return([]agent.Message{{Role: "user", Content: "记住"}}, nil)
	env.OnActivity("BuildSystemPrompt", mock.Anything, "s1").Return("", nil)
	env.OnActivity("CallLLM", mock.Anything, mock.Anything).
		Return(agent.StepResult{ToolCalls: []agent.ToolCall{tc}}, nil).Once()
	env.OnActivity("CallLLM", mock.Anything, mock.Anything).Return(agent.StepResult{Text: "记下了"}, nil).Once()
	env.OnActivity("ExecMemoryTool", mock.Anything, mock.Anything).
		Return(`{"saved":true,"applied":1}`, nil).Once()
	env.OnActivity("WriteMessage", mock.Anything, mock.Anything).Return(int64(1), nil)
	env.OnActivity("PublishChanged", mock.Anything, "s1").Return(nil)
	env.OnActivity("StartMemoryPass", mock.Anything, "s1", mock.Anything, mock.Anything).Return(nil)
	env.OnActivity("ReleaseTurn", mock.Anything, "s1", mock.Anything).Return(nil)
	// EnsureAwake / ExecTool 故意不 mock：被调即测试失败

	env.ExecuteWorkflow(TurnWorkflow, TurnInput{SessionID: "s1"})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	env.AssertExpectations(t)
}

// ExecMemoryTool 真 PG：save 立即落库 + 审计 source=agent（验收 4）。
func TestExecMemoryTool_SaveSearchRead(t *testing.T) {
	db, raw := newMemoryTestPG(t)
	ctx := context.Background()
	sess, err := db.CreateSession(ctx, "u-tool", "t")
	if err != nil {
		t.Fatal(err)
	}
	a := &Activities{PG: db}

	// save
	out, err := a.ExecMemoryTool(ctx, ExecMemoryToolInput{
		SessionID: sess.ID.String(), Name: "memory_save",
		Input: `{"type":"profile","text":"用户偏好简短回答","importance":9}`,
	})
	if err != nil || !strings.Contains(out, `"applied":1`) {
		t.Fatalf("save: out=%s err=%v", out, err)
	}
	mems, _ := db.ListProfileMemories(ctx, "u-tool")
	if len(mems) != 1 {
		t.Fatalf("save 后 profile = %d, want 1", len(mems))
	}
	var src string
	if err := raw.QueryRow(ctx,
		"SELECT source FROM memory_ops WHERE user_id='u-tool'").Scan(&src); err != nil {
		t.Fatal(err)
	}
	if src != "agent" {
		t.Fatalf("审计 source = %q, want agent（验收 4）", src)
	}

	// search
	out, err = a.ExecMemoryTool(ctx, ExecMemoryToolInput{
		SessionID: sess.ID.String(), Name: "memory_search", Input: `{"query":"简短","limit":5}`,
	})
	if err != nil || !strings.Contains(out, "偏好简短") {
		t.Fatalf("search: out=%s err=%v", out, err)
	}

	// read（search 返回的 id 能读回）
	out, err = a.ExecMemoryTool(ctx, ExecMemoryToolInput{
		SessionID: sess.ID.String(), Name: "memory_read",
		Input: `{"kind":"memory","id":"` + mems[0].ID.String() + `"}`,
	})
	if err != nil || !strings.Contains(out, "偏好简短") {
		t.Fatalf("read: out=%s err=%v", out, err)
	}
}

func TestExecMemoryTool_Validation(t *testing.T) {
	db, _ := newMemoryTestPG(t)
	ctx := context.Background()
	sess, _ := db.CreateSession(ctx, "u-val", "t")
	a := &Activities{PG: db}

	// 校验/用法错误 = 模型可见的 exit_code=-1 输出，不是 activity error（后者被无限重试会卡死 turn）
	for _, in := range []ExecMemoryToolInput{
		{SessionID: sess.ID.String(), Name: "memory_nuke", Input: `{}`},                         // 未知工具
		{SessionID: sess.ID.String(), Name: "memory_save", Input: `{"type":"profile"}`},         // 缺 text
		{SessionID: sess.ID.String(), Name: "memory_search", Input: `{"query":""}`},             // 空 query
		{SessionID: sess.ID.String(), Name: "memory_read", Input: `{"kind":"memory","id":"x"}`}, // 坏 uuid
	} {
		out, err := a.ExecMemoryTool(ctx, in)
		if err != nil {
			t.Fatalf("%s 校验错误不应是 activity error: %v", in.Name, err)
		}
		if !strings.Contains(out, `"exit_code":-1`) {
			t.Fatalf("%s 应返回 exit_code=-1: %s", in.Name, out)
		}
	}
}
