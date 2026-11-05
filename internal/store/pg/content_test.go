package pg

import (
	"encoding/json"
	"testing"
)

// content 编解码的对称性：marshal 后 parse 必须原样读回。
// 与 loadhistory_test.go 的字面量锚点互补——那边钉存储形状，这边钉函数互逆。
func TestContentCodec_RoundTrip(t *testing.T) {
	if got := ParseText(MarshalText("你好")); got != "你好" {
		t.Errorf("text round-trip = %q", got)
	}
	// ParseText 兜底：非 {"text":...} 形状原样返回（旧 extractText 语义）
	if got := ParseText([]byte(`{"other":1}`)); got != `{"other":1}` {
		t.Errorf("ParseText fallback = %q", got)
	}

	callID, name, output, ok := ParseToolResult(MarshalToolResult("call_1", "shell", `{"exit_code":0}`))
	if !ok || callID != "call_1" || name != "shell" || output != `{"exit_code":0}` {
		t.Errorf("tool_result round-trip = %q %q %q ok=%v", callID, name, output, ok)
	}
	if _, _, _, ok := ParseToolResult([]byte(`not json`)); ok {
		t.Error("ParseToolResult 对非法 json 应 ok=false")
	}
}

// 新形状钉死：一个 assistant 回合 = 一行 {"text","calls"}。
// 形状是存储契约（线上 jsonb 已有数据），必须逐字节断言，不能只测互逆。
func TestMarshalToolStep_Shape(t *testing.T) {
	got := string(MarshalToolStep(ToolStep{
		Text: "我来跑",
		Calls: []ToolCall{
			{ID: "c1", Name: "shell", Input: `{"cmd":"ls"}`},
			{ID: "c2", Name: "shell", Input: `{"cmd":"pwd"}`},
		},
	}))
	var v struct {
		Text  string `json:"text"`
		Calls []struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			Input string `json:"input"`
		} `json:"calls"`
	}
	if err := json.Unmarshal([]byte(got), &v); err != nil {
		t.Fatalf("marshal 出了非法 json: %s", got)
	}
	if v.Text != "我来跑" || len(v.Calls) != 2 || v.Calls[1].Input != `{"cmd":"pwd"}` {
		t.Fatalf("shape = %s", got)
	}
}

// 纯工具回合（模型没说话）：text 必须省略，不要写 "text":""。
func TestMarshalToolStep_NoTextOmitsKey(t *testing.T) {
	got := string(MarshalToolStep(ToolStep{Calls: []ToolCall{{ID: "c1", Name: "shell", Input: `{}`}}}))
	var v map[string]json.RawMessage
	if err := json.Unmarshal([]byte(got), &v); err != nil {
		t.Fatal(err)
	}
	if _, exists := v["text"]; exists {
		t.Fatalf("空 text 不应出现字段: %s", got)
	}
}

func TestParseToolStep(t *testing.T) {
	t.Run("新形状多调用", func(t *testing.T) {
		step, ok := ParseToolStep([]byte(`{"text":"跑两条","calls":[` +
			`{"id":"c1","name":"shell","input":"{\"cmd\":\"a\"}"},` +
			`{"id":"c2","name":"shell","input":"{\"cmd\":\"b\"}"}]}`))
		if !ok || step.Text != "跑两条" || len(step.Calls) != 2 || step.Calls[0].ID != "c1" {
			t.Fatalf("step = %+v ok=%v", step, ok)
		}
	})

	// 线上老数据是一条调用一行；解析必须还能还原成单调用回合。
	t.Run("旧形状单调用", func(t *testing.T) {
		step, ok := ParseToolStep([]byte(`{"id":"c1","name":"shell","input":"{\"cmd\":\"ls\"}"}`))
		if !ok || step.Text != "" || len(step.Calls) != 1 || step.Calls[0].Name != "shell" {
			t.Fatalf("step = %+v ok=%v", step, ok)
		}
	})

	t.Run("非法 json", func(t *testing.T) {
		if _, ok := ParseToolStep([]byte(`not json`)); ok {
			t.Fatal("应 ok=false")
		}
	})

	// 空对象既没有调用也没有文本：应视为坏行跳过，而不是造一条空 assistant。
	t.Run("空对象", func(t *testing.T) {
		if _, ok := ParseToolStep([]byte(`{}`)); ok {
			t.Fatal("{} 应 ok=false")
		}
	})
}
