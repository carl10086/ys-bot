// 消息 content（jsonb）形状的唯一归属。
// kind 词汇见 pg.go（KindUser 等）；这里管每种 kind 的 content 编解码。
// 写方（api 发用户消息、workflow 写 assistant/tool 消息）与读方（workflow 历史投影）
// 都必须用这组函数——形状改动只动这一个文件。
package pg

import "encoding/json"

// MarshalText user/assistant 文本消息的 content：{"text": "..."}。
func MarshalText(text string) []byte {
	b, _ := json.Marshal(map[string]string{"text": text})
	return b
}

// ParseText 解析 {"text": "..."}；形状不符（含空 text）原样返回 content 字符串。
func ParseText(content []byte) string {
	var v struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &v) == nil && v.Text != "" {
		return v.Text
	}
	return string(content)
}

// MarshalToolCall kind=tool_call 的 content：{"id","name","input"}。
func MarshalToolCall(id, name, input string) []byte {
	b, _ := json.Marshal(struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Input string `json:"input"`
	}{id, name, input})
	return b
}

// ParseToolCall 是 MarshalToolCall 的逆；json 解析失败返回 ok=false（调用方跳过该行）。
func ParseToolCall(content []byte) (id, name, input string, ok bool) {
	var v struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Input string `json:"input"`
	}
	if json.Unmarshal(content, &v) != nil {
		return "", "", "", false
	}
	return v.ID, v.Name, v.Input, true
}

// MarshalToolResult kind=tool_result 的 content：{"tool_call_id","name","output"}。
func MarshalToolResult(callID, name, output string) []byte {
	b, _ := json.Marshal(struct {
		ToolCallID string `json:"tool_call_id"`
		Name       string `json:"name"`
		Output     string `json:"output"`
	}{callID, name, output})
	return b
}

// ParseToolResult 是 MarshalToolResult 的逆；json 解析失败返回 ok=false（调用方跳过该行）。
func ParseToolResult(content []byte) (callID, name, output string, ok bool) {
	var v struct {
		ToolCallID string `json:"tool_call_id"`
		Name       string `json:"name"`
		Output     string `json:"output"`
	}
	if json.Unmarshal(content, &v) != nil {
		return "", "", "", false
	}
	return v.ToolCallID, v.Name, v.Output, true
}
