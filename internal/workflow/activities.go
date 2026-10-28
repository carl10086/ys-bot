package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/google/uuid"

	"ys-sandbox/internal/agent"
	"ys-sandbox/internal/store/pg"
	"ys-sandbox/internal/store/redis"
)

// Activities TurnWorkflow 的全部副作用。每步落库 / 空信号 = 讲座 15 黑条的实现。
type Activities struct {
	PG    *pg.PG
	Redis *redis.Redis
	Agent agent.Agent
}

// DrainQueue 取走会话队列里待处理的一批消息（seq 列表）。队列可丢：消息已在 PG。
func (a *Activities) DrainQueue(ctx context.Context, sessionID string) ([]int64, error) {
	raw, err := a.Redis.DrainQueue(ctx, redis.Queue{SessionID: sessionID})
	if err != nil {
		return nil, err
	}
	out := make([]int64, 0, len(raw))
	for _, s := range raw {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("queue payload %q not a seq: %w", s, err)
		}
		out = append(out, n)
	}
	return out, nil
}

// LoadHistory 从 PG 读会话全部消息，映射为 agent 历史（M1：只有 user/assistant 两种 kind）。
func (a *Activities) LoadHistory(ctx context.Context, sessionID string) ([]agent.Message, error) {
	sid, err := uuid.Parse(sessionID)
	if err != nil {
		return nil, err
	}
	msgs, err := a.PG.ListMessages(ctx, sid, 0)
	if err != nil {
		return nil, err
	}
	out := make([]agent.Message, 0, len(msgs))
	for _, m := range msgs {
		role := ""
		switch m.Kind {
		case pg.KindUser:
			role = "user"
		case pg.KindAssistant:
			role = "assistant"
		default:
			continue // tool_call/tool_result 在 M2 才有意义
		}
		out = append(out, agent.Message{Role: role, Content: extractText(m.Content)})
	}
	return out, nil
}

func (a *Activities) CallLLM(ctx context.Context, history []agent.Message) (string, error) {
	return a.Agent.Reply(ctx, history)
}

func (a *Activities) WriteMessage(ctx context.Context, in WriteMessageInput) (int64, error) {
	sid, err := uuid.Parse(in.SessionID)
	if err != nil {
		return 0, err
	}
	content, _ := json.Marshal(map[string]string{"text": in.Text})
	seq, _, err := a.PG.InsertMessage(ctx, pg.Message{
		SessionID: sid, Kind: in.Kind, Content: content, TurnID: in.TurnID,
	})
	return seq, err
}

func (a *Activities) PublishChanged(ctx context.Context, sessionID string) error {
	return a.Redis.PublishChanged(ctx, sessionID)
}

func (a *Activities) ReleaseTurn(ctx context.Context, sessionID string) error {
	return a.Redis.ReleaseTurn(ctx, sessionID)
}

// content jsonb 当前只有一种形状 {"text": "..."}；解析失败时原样返回。
func extractText(content []byte) string {
	var v struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &v) == nil && v.Text != "" {
		return v.Text
	}
	return string(content)
}
