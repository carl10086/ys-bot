package workflow

import (
	"context"
	"os"
	"testing"

	"ys-sandbox/internal/agent"
	"ys-sandbox/internal/store/pg"
)

// LoadHistory 集成测试的 PG：独立库 ys_sandbox_test_wf（go test 包间并行，
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

// 钉住消息形状的读写闭环（重构前的行为锚点）：
// 按 turn.go 实际写库的四种 content 形状插入 PG，LoadHistory 必须投影回等价的 agent 历史。
// 改任何一侧的形状（写方 turn.go / 读方 LoadHistory）都会让这个测试红。
func TestLoadHistory_RoundTrip(t *testing.T) {
	p := newTestPG(t)
	ctx := context.Background()

	sess, err := p.CreateSession(ctx, "local", "")
	if err != nil {
		t.Fatal(err)
	}
	sid := sess.ID.String()

	// 与 turn.go 写入形状一一对应的四条消息
	insert := func(kind, content string) {
		t.Helper()
		if _, _, err := p.InsertMessage(ctx, pg.Message{
			SessionID: sess.ID, Kind: kind, Content: []byte(content),
		}); err != nil {
			t.Fatalf("insert %s: %v", kind, err)
		}
	}
	insert(pg.KindUser, `{"text":"看看 /etc"}`)
	insert(pg.KindToolCall, `{"id":"call_1","name":"shell","input":"{\"cmd\":\"ls /etc\"}"}`)
	insert(pg.KindToolResult, `{"tool_call_id":"call_1","name":"shell","output":"{\"exit_code\":0,\"stdout\":\"hosts\"}"}`)
	insert(pg.KindAssistant, `{"text":"/etc 下有 hosts"}`)

	got, err := (&Activities{PG: p}).LoadHistory(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}

	want := []agent.Message{
		{Role: "user", Content: "看看 /etc"},
		{Role: "assistant", ToolCalls: []agent.ToolCall{{ID: "call_1", Name: "shell", Input: `{"cmd":"ls /etc"}`}}},
		{Role: "tool", ToolCallID: "call_1", Name: "shell", Content: `{"exit_code":0,"stdout":"hosts"}`},
		{Role: "assistant", Content: "/etc 下有 hosts"},
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
		} else if len(w.ToolCalls) == 1 && g.ToolCalls[0] != w.ToolCalls[0] {
			t.Errorf("msg[%d] toolcall = %+v, want %+v", i, g.ToolCalls[0], w.ToolCalls[0])
		}
	}
}
