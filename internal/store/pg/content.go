// 消息 content（jsonb）形状的唯一归属。
// kind 词汇见 pg.go（KindUser 等）；这里管每种 kind 的 content 编解码。
// 写方（api 发用户消息、workflow 写 assistant/tool 消息）与读方（workflow 历史投影）
// 都必须用这组函数——形状改动只动这一个文件。
package pg

import "encoding/json"

// 三种 content 形状各一个 struct，marshal/unmarshal 共用（字段改动只动一处）。

// textContent user/assistant 文本消息：{"text": "..."}。
type textContent struct {
	Text string `json:"text"`
}

// ToolCall 一次工具调用。中性结构：pg 包不 import agent，由调用方转换。
type ToolCall struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Input string `json:"input"`
}

// ToolStep 一个 assistant 回合：文本 + 该回合发出的全部工具调用。
// 一步一行——同一回合的多个调用必须同行，否则回灌 provider 时
// tool_use / tool_result 的配对关系会错位。
type ToolStep struct {
	Text  string
	Calls []ToolCall
}

// toolStepWire 是 ToolStep 的存储形状。
// 保留顶层 id/name/input 字段是为了读 2026-11-05 之前"一次调用一行"的老数据。
type toolStepWire struct {
	Text  string     `json:"text,omitempty"`
	Calls []ToolCall `json:"calls,omitempty"`
	ID    string     `json:"id,omitempty"`
	Name  string     `json:"name,omitempty"`
	Input string     `json:"input,omitempty"`
}

// toolResultContent kind=tool_result：{"tool_call_id","name","output"}。
type toolResultContent struct {
	ToolCallID string `json:"tool_call_id"`
	Name       string `json:"name"`
	Output     string `json:"output"`
}

// MarshalText user/assistant 文本消息的 content。
func MarshalText(text string) []byte {
	b, _ := json.Marshal(textContent{Text: text})
	return b
}

// ParseText 解析 {"text": "..."}；形状不符（含空 text）原样返回 content 字符串。
func ParseText(content []byte) string {
	var v textContent
	if json.Unmarshal(content, &v) == nil && v.Text != "" {
		return v.Text
	}
	return string(content)
}

// MarshalToolStep kind=tool_call 的 content：{"text","calls"}，空 text 不落库。
func MarshalToolStep(step ToolStep) []byte {
	b, _ := json.Marshal(toolStepWire{Text: step.Text, Calls: step.Calls})
	return b
}

// ParseToolStep 是 MarshalToolStep 的逆，并兼容旧的单调用形状。
// 既无调用也无标识的坏行返回 ok=false（调用方跳过，不造空 assistant）。
func ParseToolStep(content []byte) (ToolStep, bool) {
	var v toolStepWire
	if json.Unmarshal(content, &v) != nil {
		return ToolStep{}, false
	}
	if len(v.Calls) > 0 {
		return ToolStep{Text: v.Text, Calls: v.Calls}, true
	}
	if v.ID != "" || v.Name != "" {
		return ToolStep{Calls: []ToolCall{{ID: v.ID, Name: v.Name, Input: v.Input}}}, true
	}
	return ToolStep{}, false
}

// MarshalToolResult kind=tool_result 的 content。
func MarshalToolResult(callID, name, output string) []byte {
	b, _ := json.Marshal(toolResultContent{callID, name, output})
	return b
}

// ParseToolResult 是 MarshalToolResult 的逆；json 解析失败返回 ok=false（调用方跳过该行）。
func ParseToolResult(content []byte) (callID, name, output string, ok bool) {
	var v toolResultContent
	if json.Unmarshal(content, &v) != nil {
		return "", "", "", false
	}
	return v.ToolCallID, v.Name, v.Output, true
}
