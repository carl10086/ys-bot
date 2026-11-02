package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 假 OpenAI 兼容端点：回显最后一条 user 消息的文本。
// 验证点：① 适配层把历史正确映射为 OpenAI messages ② 返回文本被正确读出。
func TestAgent_Step_MapsHistoryAndReturnsText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		lastUser := ""
		for _, m := range req.Messages {
			if m.Role != "user" {
				continue
			}
			// content 可能是纯字符串或 [{"type":"text","text":...}] 分片
			var s string
			if json.Unmarshal(m.Content, &s) == nil {
				lastUser = s
				continue
			}
			var parts []struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(m.Content, &parts) == nil && len(parts) > 0 {
				lastUser = parts[0].Text
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-test", "object": "chat.completion", "created": 1, "model": "test-model",
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "stop",
				"message": map[string]string{"role": "assistant", "content": "echo:" + lastUser},
			}},
			"usage": map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	}))
	defer srv.Close()

	a, err := NewFantasy(Config{BaseURL: srv.URL, APIKey: "fake", Model: "test-model", System: "你是助手"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.Step(context.Background(), []Message{
		{Role: "user", Content: "第一条"},
		{Role: "assistant", Content: "回复一"},
		{Role: "user", Content: "最新问题"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "echo:最新问题" {
		t.Fatalf("text=%q, want echo:最新问题", res.Text)
	}
	if len(res.ToolCalls) != 0 {
		t.Fatalf("无工具请求不应产生 ToolCalls: %+v", res.ToolCalls)
	}
}

// M5：history 里的 role=system 消息必须映射为 OpenAI 的 system 消息（注入通道）。
func TestAgent_Step_SystemRoleFromHistory(t *testing.T) {
	var gotSystem string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		for _, m := range req.Messages {
			if m.Role == "system" {
				gotSystem = m.Content
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv.Close()

	ag, err := NewFantasy(Config{Provider: "openai", BaseURL: srv.URL + "/v1", APIKey: "k", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = ag.Step(context.Background(), []Message{
		{Role: "system", Content: "记忆块：偏好简短"},
		{Role: "user", Content: "hi"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if gotSystem != "记忆块：偏好简短" {
		t.Fatalf("system 消息未映射: %q", gotSystem)
	}
}
