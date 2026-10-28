// Package agent 是 LLM 的薄适配层。
// 红线：fantasy 类型被关在本包内，业务代码不得 import charm.land/fantasy。
// M1 只做一次普通补全（无工具循环）；工具循环 M2 引入。
package agent

import (
	"context"
	"fmt"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/openaicompat"
)

// Message 一条对话消息（对外中性类型，不泄漏 fantasy）。
type Message struct {
	Role    string // "user" / "assistant"
	Content string
}

// Agent 一个 turn 内被调用一次：给定对话历史，产出回复文本。
type Agent interface {
	Reply(ctx context.Context, history []Message) (string, error)
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

func (a *fantasyAgent) Reply(ctx context.Context, history []Message) (string, error) {
	ag := fantasy.NewAgent(a.model, fantasy.WithSystemPrompt(a.system))
	msgs := make([]fantasy.Message, 0, len(history))
	for _, m := range history {
		switch m.Role {
		case "user":
			msgs = append(msgs, fantasy.NewUserMessage(m.Content))
		case "assistant":
			msgs = append(msgs, fantasy.Message{
				Role:    fantasy.MessageRoleAssistant,
				Content: []fantasy.MessagePart{fantasy.TextPart{Text: m.Content}},
			})
		default:
			return "", fmt.Errorf("unknown role %q", m.Role)
		}
	}
	res, err := ag.Generate(ctx, fantasy.AgentCall{Messages: msgs})
	if err != nil {
		return "", err
	}
	return res.Response.Content.Text(), nil
}
