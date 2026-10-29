// Package agent 是 LLM 的薄适配层。
// 红线：fantasy 类型被关在本包内，业务代码不得 import charm.land/fantasy。
// M2 起支持工具调用：Step = 模型的一次决策（要么回文本，要么请求一批工具调用）；
// 工具 loop 在 Temporal workflow 里（每步落库），不用 fantasy 内置 agent loop。
package agent

import (
	"context"
	"fmt"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/openaicompat"
)

// Message 一条对话消息（对外中性类型，不泄漏 fantasy）。
// role=assistant 可携带 ToolCalls；role=tool 是某次调用的结果（ToolCallID 对应）。
type Message struct {
	Role       string // "user" / "assistant" / "tool"
	Content    string
	ToolCalls  []ToolCall
	ToolCallID string
	Name       string
}

// ToolCall 模型请求执行一次工具。Input 是 JSON 字符串（与 schema 匹配）。
type ToolCall struct {
	ID    string
	Name  string
	Input string
}

// ToolDef 工具的 schema 定义（定义在 server，讲座 s19）。
type ToolDef struct {
	Name        string
	Description string
	InputSchema map[string]any
}

// StepResult 一次模型决策的产出：要么最终文本，要么一批工具调用。
type StepResult struct {
	Text      string
	ToolCalls []ToolCall
}

// Agent 每 turn 被 workflow 反复调用：给定历史，产出一步决策。
type Agent interface {
	Step(ctx context.Context, history []Message, tools []ToolDef) (StepResult, error)
}

type Config struct {
	Provider string // "openai"（默认，OpenAI 兼容）/ "anthropic"
	BaseURL  string // 兼容端点（自建网关/官方均可）
	APIKey   string
	Model    string
	System   string // 系统提示词
}

type fantasyAgent struct {
	model  fantasy.LanguageModel
	system string
}

func NewFantasy(cfg Config) (Agent, error) {
	var provider fantasy.Provider
	var err error
	if cfg.Provider == "anthropic" {
		provider, err = anthropic.New(
			anthropic.WithBaseURL(cfg.BaseURL),
			anthropic.WithAPIKey(cfg.APIKey),
		)
	} else {
		provider, err = openaicompat.New(
			openaicompat.WithBaseURL(cfg.BaseURL),
			openaicompat.WithAPIKey(cfg.APIKey),
		)
	}
	if err != nil {
		return nil, fmt.Errorf("%s provider: %w", cfg.Provider, err)
	}
	model, err := provider.LanguageModel(context.Background(), cfg.Model)
	if err != nil {
		return nil, fmt.Errorf("language model %q: %w", cfg.Model, err)
	}
	return &fantasyAgent{model: model, system: cfg.System}, nil
}

func (a *fantasyAgent) Step(ctx context.Context, history []Message, tools []ToolDef) (StepResult, error) {
	prompt := make(fantasy.Prompt, 0, len(history)+1)
	if a.system != "" {
		prompt = append(prompt, fantasy.Message{
			Role:    fantasy.MessageRoleSystem,
			Content: []fantasy.MessagePart{fantasy.TextPart{Text: a.system}},
		})
	}
	for _, m := range history {
		switch m.Role {
		case "user":
			prompt = append(prompt, fantasy.NewUserMessage(m.Content))
		case "assistant":
			parts := make([]fantasy.MessagePart, 0, 1+len(m.ToolCalls))
			if m.Content != "" {
				parts = append(parts, fantasy.TextPart{Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				parts = append(parts, fantasy.ToolCallPart{ToolCallID: tc.ID, ToolName: tc.Name, Input: tc.Input})
			}
			prompt = append(prompt, fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: parts})
		case "tool":
			prompt = append(prompt, fantasy.Message{
				Role: fantasy.MessageRoleTool,
				Content: []fantasy.MessagePart{fantasy.ToolResultPart{
					ToolCallID: m.ToolCallID,
					Output:     fantasy.ToolResultOutputContentText{Text: m.Content},
				}},
			})
		default:
			return StepResult{}, fmt.Errorf("unknown role %q", m.Role)
		}
	}

	fTools := make([]fantasy.Tool, 0, len(tools))
	for _, t := range tools {
		fTools = append(fTools, fantasy.FunctionTool{
			Name: t.Name, Description: t.Description, InputSchema: t.InputSchema,
		})
	}
	res, err := a.model.Generate(ctx, fantasy.Call{Prompt: prompt, Tools: fTools})
	if err != nil {
		return StepResult{}, err
	}
	out := StepResult{Text: res.Content.Text()}
	for _, tc := range res.Content.ToolCalls() {
		out.ToolCalls = append(out.ToolCalls, ToolCall{ID: tc.ToolCallID, Name: tc.ToolName, Input: tc.Input})
	}
	return out, nil
}
