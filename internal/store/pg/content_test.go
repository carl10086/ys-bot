package pg

import "testing"

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

	id, name, input, ok := ParseToolCall(MarshalToolCall("call_1", "shell", `{"cmd":"ls"}`))
	if !ok || id != "call_1" || name != "shell" || input != `{"cmd":"ls"}` {
		t.Errorf("tool_call round-trip = %q %q %q ok=%v", id, name, input, ok)
	}
	if _, _, _, ok := ParseToolCall([]byte(`not json`)); ok {
		t.Error("ParseToolCall 对非法 json 应 ok=false")
	}

	callID, name, output, ok := ParseToolResult(MarshalToolResult("call_1", "shell", `{"exit_code":0}`))
	if !ok || callID != "call_1" || name != "shell" || output != `{"exit_code":0}` {
		t.Errorf("tool_result round-trip = %q %q %q ok=%v", callID, name, output, ok)
	}
	if _, _, _, ok := ParseToolResult([]byte(`not json`)); ok {
		t.Error("ParseToolResult 对非法 json 应 ok=false")
	}
}
