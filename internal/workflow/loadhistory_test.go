package workflow

import (
	"context"
	"os"
	"testing"

	"ys-bot/internal/agent"
	"ys-bot/internal/store/pg"
)

// 本包集成测试的 PG：独立库 ys_sandbox_test_wf（go test 包间并行，
// 与 store/pg、api 的测试库错开——本包 ResetSchema 不会撞别人）。
func newTestPG(t *testing.T) *pg.PG {
	t.Helper()
	dsn := os.Getenv("YS_TEST_PG_WF")
	if dsn == "" {
		dsn = "postgres://ys:ys@localhost:5432/ys_sandbox_test_wf"
	}
	p, err := pg.New(context.Background(), dsn)
	if err != nil {
		t.Skipf("PG 不可用（YS_TEST_PG_WF 指向可用实例）：%v", err)
	}
	if err := p.ResetSchema(context.Background()); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// 钉住消息形状的读写闭环：按 turn.go 实际写库的形状插入 PG，
// loadHistory 必须投影回等价的 agent 历史。
func TestLoadHistory_RoundTrip(t *testing.T) {
	p := newTestPG(t)
	ctx := context.Background()

	sess, err := p.CreateSession(ctx, "local", "")
	if err != nil {
		t.Fatal(err)
	}
	sid := sess.ID.String()

	insert := func(kind string, content []byte) {
		t.Helper()
		if _, _, err := p.InsertMessage(ctx, pg.Message{
			SessionID: sess.ID, Kind: kind, Content: content,
		}); err != nil {
			t.Fatalf("insert %s: %v", kind, err)
		}
	}
	insert(pg.KindUser, pg.MarshalText("跑两条命令"))
	// 新形状：一个 assistant 回合一行，带两个调用
	insert(pg.KindToolCall, pg.MarshalToolStep(pg.ToolStep{
		Text: "我来跑",
		Calls: []pg.ToolCall{
			{ID: "c1", Name: "shell", Input: `{"cmd":"echo a"}`},
			{ID: "c2", Name: "shell", Input: `{"cmd":"echo b"}`},
		},
	}))
	insert(pg.KindToolResult, pg.MarshalToolResult("c1", "shell", `{"exit_code":0,"stdout":"a"}`))
	insert(pg.KindToolResult, pg.MarshalToolResult("c2", "shell", `{"exit_code":0,"stdout":"b"}`))
	// 旧形状：老会话里一次调用一行，必须还能读
	insert(pg.KindToolCall, []byte(`{"id":"c3","name":"shell","input":"{\"cmd\":\"pwd\"}"}`))
	insert(pg.KindToolResult, pg.MarshalToolResult("c3", "shell", `{"exit_code":0,"stdout":"/root"}`))

	got, err := (&Activities{PG: p}).loadHistory(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}

	want := []agent.Message{
		{Role: "user", Content: "跑两条命令"},
		{Role: "assistant", Content: "我来跑", ToolCalls: []agent.ToolCall{
			{ID: "c1", Name: "shell", Input: `{"cmd":"echo a"}`},
			{ID: "c2", Name: "shell", Input: `{"cmd":"echo b"}`},
		}},
		{Role: "tool", ToolCallID: "c1", Name: "shell", Content: `{"exit_code":0,"stdout":"a"}`},
		{Role: "tool", ToolCallID: "c2", Name: "shell", Content: `{"exit_code":0,"stdout":"b"}`},
		{Role: "assistant", ToolCalls: []agent.ToolCall{{ID: "c3", Name: "shell", Input: `{"cmd":"pwd"}`}}},
		{Role: "tool", ToolCallID: "c3", Name: "shell", Content: `{"exit_code":0,"stdout":"/root"}`},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d messages, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		g := got[i]
		if g.Role != w.Role || g.Content != w.Content || g.ToolCallID != w.ToolCallID || g.Name != w.Name {
			t.Errorf("msg[%d] = %+v, want %+v", i, g, w)
		}
		if len(g.ToolCalls) != len(w.ToolCalls) {
			t.Errorf("msg[%d] toolcalls = %+v, want %+v", i, g.ToolCalls, w.ToolCalls)
		}
	}
}

// 记忆注入：system prompt 必须是 history[0]（不落库）。
func TestLoadHistory_SystemPromptFirst(t *testing.T) {
	p := newTestPG(t)
	ctx := context.Background()
	sess, err := p.CreateSession(ctx, "u-inject", "t")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.InsertMessage(ctx, pg.Message{
		SessionID: sess.ID, Kind: pg.KindUser, Content: pg.MarshalText("你好"),
	}); err != nil {
		t.Fatal(err)
	}
	a := &Activities{PG: p, BasePrompt: "BASE"}
	got, err := a.loadHistory(ctx, sess.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Role != "system" || got[0].Content != "BASE" || got[1].Role != "user" {
		t.Fatalf("history = %+v", got)
	}
}

// 注入失败降级（review I3）：PG 抖动不该搞挂整个对话——记忆是增强不是依赖。
func TestLoadHistory_PromptFailureDegrades(t *testing.T) {
	p := newTestPG(t)
	ctx := context.Background()
	// 不存在的 session：ListMessages 返回空，systemPrompt 在 GetSession 处失败 → 降级
	got, err := (&Activities{PG: p}).loadHistory(ctx, "018f0000-0000-7000-8000-000000000001")
	if err != nil {
		t.Fatalf("降级路径不应返回错误: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("history = %+v", got)
	}
}
