package workflow

import (
	"ys-bot/internal/agent"
	"ys-bot/internal/store/pg"
)

// projectHistory PG 消息 → agent 历史投影。
// 映射规则：user/assistant 取 text；tool_call 归并为 assistant 携带的 ToolCalls；
// tool_result 变 role=tool 消息；无法解析的行跳过。content 编解码全在 pg 包（content.go）。
func projectHistory(msgs []pg.Message) []agent.Message {
	out := make([]agent.Message, 0, len(msgs))
	for _, m := range msgs {
		switch m.Kind {
		case pg.KindUser:
			out = append(out, agent.Message{Role: "user", Content: pg.ParseText(m.Content)})
		case pg.KindAssistant:
			out = append(out, agent.Message{Role: "assistant", Content: pg.ParseText(m.Content)})
		case pg.KindToolCall:
			if id, name, input, ok := pg.ParseToolCall(m.Content); ok {
				out = append(out, agent.Message{
					Role:      "assistant",
					ToolCalls: []agent.ToolCall{{ID: id, Name: name, Input: input}},
				})
			}
		case pg.KindToolResult:
			if callID, name, output, ok := pg.ParseToolResult(m.Content); ok {
				out = append(out, agent.Message{
					Role: "tool", ToolCallID: callID, Name: name, Content: output,
				})
			}
		}
	}
	return out
}
