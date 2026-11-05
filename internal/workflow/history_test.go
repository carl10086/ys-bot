package workflow

import (
	"testing"

	"ys-bot/internal/store/pg"
)

// 一个 assistant 回合里的多个 tool_call 必须投影成"一条 assistant + 若干 tool"。
// 旧实现一条调用一行，同回合的调用被拆成多条 assistant，回灌 provider 时
// tool_use / tool_result 的配对关系丢失。
func TestProjectHistory_MultiCallStep(t *testing.T) {
	msgs := []pg.Message{
		{Seq: 1, Kind: pg.KindUser, Content: pg.MarshalText("跑两条命令")},
		{Seq: 2, Kind: pg.KindToolCall, Content: pg.MarshalToolStep(pg.ToolStep{
			Text: "我来跑",
			Calls: []pg.ToolCall{
				{ID: "c1", Name: "shell", Input: `{"cmd":"echo a"}`},
				{ID: "c2", Name: "shell", Input: `{"cmd":"echo b"}`},
			},
		})},
		{Seq: 3, Kind: pg.KindToolResult, Content: pg.MarshalToolResult("c1", "shell", `{"exit_code":0,"stdout":"a"}`)},
		{Seq: 4, Kind: pg.KindToolResult, Content: pg.MarshalToolResult("c2", "shell", `{"exit_code":0,"stdout":"b"}`)},
	}

	got := projectHistory(msgs)
	if len(got) != 4 {
		t.Fatalf("want 4 条（user / assistant / tool / tool），got %d: %+v", len(got), got)
	}
	a := got[1]
	if a.Role != "assistant" || a.Content != "我来跑" || len(a.ToolCalls) != 2 {
		t.Fatalf("assistant 回合 = %+v", a)
	}
	if a.ToolCalls[0].ID != "c1" || a.ToolCalls[1].ID != "c2" {
		t.Fatalf("call ids = %+v", a.ToolCalls)
	}
	if got[2].Role != "tool" || got[2].ToolCallID != "c1" || got[3].ToolCallID != "c2" {
		t.Fatalf("tool results = %+v / %+v", got[2], got[3])
	}
}

// 旧形状（一次调用一行）的老数据必须还能读——线上已有会话。
func TestProjectHistory_LegacySingleCall(t *testing.T) {
	msgs := []pg.Message{
		{Seq: 1, Kind: pg.KindToolCall, Content: []byte(`{"id":"c1","name":"shell","input":"{\"cmd\":\"ls\"}"}`)},
	}
	got := projectHistory(msgs)
	if len(got) != 1 || len(got[0].ToolCalls) != 1 || got[0].ToolCalls[0].ID != "c1" {
		t.Fatalf("老形状 = %+v", got)
	}
}
