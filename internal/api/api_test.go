package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// 集成测试：真 PG/Redis（compose），假 TurnStarter。
// 验收点（spec §1）：幂等重发、空闲起 turn、turn 进行中打断合并。

type fakeStarter struct {
	started  []string // workflowID
	canceled []string // workflowID
}

func (f *fakeStarter) StartTurn(_ context.Context, workflowID, _ string) error {
	f.started = append(f.started, workflowID)
	return nil
}
func (f *fakeStarter) CancelTurn(_ context.Context, workflowID string) error {
	f.canceled = append(f.canceled, workflowID)
	return nil
}

func TestAPI_SendMessage_Dedup(t *testing.T) {
	srv, _, _, cleanup := newTestServer(t)
	defer cleanup()
	sid := mustCreateSessionViaAPI(t, srv)

	body := `{"msg_id":"` + uuid.NewString() + `","text":"你好"}`
	r1 := postJSON(t, srv, "/api/sessions/"+sid+"/messages", body)
	if r1.Deduped {
		t.Fatal("first send must not be deduped")
	}
	r2 := postJSON(t, srv, "/api/sessions/"+sid+"/messages", body)
	if !r2.Deduped || r2.Seq != r1.Seq {
		t.Fatalf("redeliver: got %+v want deduped seq=%d", r2, r1.Seq)
	}

	msgs := getMessages(t, srv, sid, 0)
	if len(msgs) != 1 {
		t.Fatalf("want 1 message after redeliver, got %d", len(msgs))
	}
}

func TestAPI_SendMessage_StartsTurnWhenIdle(t *testing.T) {
	srv, starter, _, cleanup := newTestServer(t)
	defer cleanup()
	sid := mustCreateSessionViaAPI(t, srv)

	postJSON(t, srv, "/api/sessions/"+sid+"/messages", `{"msg_id":"`+uuid.NewString()+`","text":"hi"}`)

	if len(starter.started) != 1 || !strings.HasPrefix(starter.started[0], "turn:"+sid+":") {
		t.Fatalf("started=%v, want workflowID 形如 turn:{sid}:{uuid}", starter.started)
	}
	if len(starter.canceled) != 0 {
		t.Fatalf("idle session must not cancel, got %v", starter.canceled)
	}
}

func TestAPI_SendMessage_InterruptsRunningTurn(t *testing.T) {
	srv, starter, _, cleanup := newTestServer(t)
	defer cleanup()
	sid := mustCreateSessionViaAPI(t, srv)

	postJSON(t, srv, "/api/sessions/"+sid+"/messages", `{"msg_id":"`+uuid.NewString()+`","text":"第一条"}`)
	firstWF := starter.started[0]

	// turn 进行中（锁被 firstWF 持有）再发 → 打断旧 turn，起新 turn
	postJSON(t, srv, "/api/sessions/"+sid+"/messages", `{"msg_id":"`+uuid.NewString()+`","text":"改主意了"}`)

	if len(starter.canceled) != 1 || starter.canceled[0] != firstWF {
		t.Fatalf("canceled=%v, want [%s]", starter.canceled, firstWF)
	}
	if len(starter.started) != 2 {
		t.Fatalf("started=%v, want 2 turns", starter.started)
	}
	// 两条消息都必须在队列里（合并进新 turn 的批量）
	msgs := getMessages(t, srv, sid, 0)
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages, got %d", len(msgs))
	}
}

func TestAPI_ListMessages_AfterSeq(t *testing.T) {
	srv, _, _, cleanup := newTestServer(t)
	defer cleanup()
	sid := mustCreateSessionViaAPI(t, srv)
	for i := 0; i < 3; i++ {
		postJSON(t, srv, "/api/sessions/"+sid+"/messages", `{"msg_id":"`+uuid.NewString()+`","text":"m"}`)
	}
	msgs := getMessages(t, srv, sid, 2)
	if len(msgs) != 1 || msgs[0].Seq != 3 {
		t.Fatalf("after_seq=2 want [seq3], got %+v", msgs)
	}
}

func TestAPI_Events_SSEEmptySignal(t *testing.T) {
	srv, _, rdb, cleanup := newTestServer(t)
	defer cleanup()
	sid := mustCreateSessionViaAPI(t, srv)

	req, _ := http.NewRequest("GET", srv.URL+"/api/sessions/"+sid+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type=%q", ct)
	}

	// 触发真实变更信号（发布者是 workflow/worker，这里直接发到同一频道）。
	// 发布是幂等空信号：循环重发直到收到，兼容远端 Redis 的订阅建立延迟。
	buf := make([]byte, 128)
	done := make(chan string, 1)
	go func() {
		n, _ := resp.Body.Read(buf)
		done <- string(buf[:n])
	}()
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case got := <-done:
			if !strings.HasPrefix(got, "data:") {
				t.Fatalf("want SSE data frame, got %q", got)
			}
			return
		case <-tick.C:
			if err := rdb.PublishChanged(context.Background(), sid); err != nil {
				t.Fatal(err)
			}
		case <-deadline:
			t.Fatal("5s 内没收到 SSE 信号")
		}
	}
}

func TestAPI_ListSessions_OrderedByRecentActivity(t *testing.T) {
	srv, _, _, cleanup := newTestServer(t)
	defer cleanup()
	s1 := mustCreateSessionViaAPI(t, srv)
	s2 := mustCreateSessionViaAPI(t, srv)

	// 初始：s2 后建，排前
	list := getSessions(t, srv, "local")
	if len(list) != 2 || list[0].ID != s2 || list[1].ID != s1 {
		t.Fatalf("initial order: %+v, want [%s %s]", list, s2, s1)
	}

	// s1 发了消息 → s1 变最近活跃，排前
	postJSON(t, srv, "/api/sessions/"+s1+"/messages", `{"msg_id":"`+uuid.NewString()+`","text":"hello"}`)
	list = getSessions(t, srv, "local")
	if len(list) != 2 || list[0].ID != s1 || list[1].ID != s2 {
		t.Fatalf("after activity: %+v, want [%s %s]", list, s1, s2)
	}

	// 别的 user 的会话不出现
	resp, err := http.Post(srv.URL+"/api/sessions", "application/json", strings.NewReader(`{"user_id":"other"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	list = getSessions(t, srv, "local")
	if len(list) != 2 {
		t.Fatalf("want 2 sessions for user local, got %d", len(list))
	}
}

func TestAPI_RenameSession(t *testing.T) {
	srv, _, _, cleanup := newTestServer(t)
	defer cleanup()
	sid := mustCreateSessionViaAPI(t, srv)

	patchJSON(t, srv, "/api/sessions/"+sid, `{"title":"改名了"}`)
	list := getSessions(t, srv, "local")
	if len(list) != 1 || list[0].Title != "改名了" {
		t.Fatalf("want renamed title, got %+v", list)
	}
}

func TestAPI_DeleteSession_RemovesSessionAndMessages(t *testing.T) {
	srv, _, _, cleanup := newTestServer(t)
	defer cleanup()
	sid := mustCreateSessionViaAPI(t, srv)
	postJSON(t, srv, "/api/sessions/"+sid+"/messages", `{"msg_id":"`+uuid.NewString()+`","text":"bye"}`)

	req, _ := http.NewRequest("DELETE", srv.URL+"/api/sessions/"+sid, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("DELETE → %d", resp.StatusCode)
	}

	// 会话 404、消息清空
	resp2, err := http.Get(srv.URL + "/api/sessions/" + sid)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 404 {
		t.Fatalf("GET deleted session → %d, want 404", resp2.StatusCode)
	}
	if msgs := getMessages(t, srv, sid, 0); len(msgs) != 0 {
		t.Fatalf("want 0 messages after delete, got %d", len(msgs))
	}
}

func TestAPI_FirstMessageAutoTitlesSession(t *testing.T) {
	srv, _, _, cleanup := newTestServer(t)
	defer cleanup()
	sid := mustCreateSessionViaAPI(t, srv)

	postJSON(t, srv, "/api/sessions/"+sid+"/messages", `{"msg_id":"`+uuid.NewString()+`","text":"帮我查一下北京明天的天气怎么样"}`)
	if got := getSessionTitle(t, srv, sid); got != "帮我查一下北京明天的天气怎么样" {
		t.Fatalf("title=%q, want 首条消息原文", got)
	}

	// 第二条消息不再改标题
	postJSON(t, srv, "/api/sessions/"+sid+"/messages", `{"msg_id":"`+uuid.NewString()+`","text":"算了不用了"}`)
	if got := getSessionTitle(t, srv, sid); got != "帮我查一下北京明天的天气怎么样" {
		t.Fatalf("title=%q, want 不被第二条消息覆盖", got)
	}
}

func TestAPI_FirstMessageAutoTitles_LongTextTruncated(t *testing.T) {
	srv, _, _, cleanup := newTestServer(t)
	defer cleanup()
	sid := mustCreateSessionViaAPI(t, srv)

	long := strings.Repeat("字", 50)
	postJSON(t, srv, "/api/sessions/"+sid+"/messages", `{"msg_id":"`+uuid.NewString()+`","text":"`+long+`"}`)
	got := getSessionTitle(t, srv, sid)
	if n := len([]rune(got)); n > 31 { // 30 字 + 省略号
		t.Fatalf("title len=%d runes, want ≤31", n)
	}
}

func TestAPI_ExplicitTitleNotOverridden(t *testing.T) {
	srv, _, _, cleanup := newTestServer(t)
	defer cleanup()
	resp, err := http.Post(srv.URL+"/api/sessions", "application/json", strings.NewReader(`{"title":"手工起的名"}`))
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		ID string `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	postJSON(t, srv, "/api/sessions/"+created.ID+"/messages", `{"msg_id":"`+uuid.NewString()+`","text":"hi"}`)
	if got := getSessionTitle(t, srv, created.ID); got != "手工起的名" {
		t.Fatalf("title=%q, want 显式标题不被覆盖", got)
	}
}

func getSessionTitle(t *testing.T, srv *httptest.Server, sid string) string {
	t.Helper()
	resp, err := http.Get(srv.URL + "/api/sessions/" + sid)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.Title
}

func getSessions(t *testing.T, srv *httptest.Server, userID string) []struct {
	ID    string `json:"id"`
	Title string `json:"title"`
} {
	t.Helper()
	resp, err := http.Get(srv.URL + "/api/sessions?user_id=" + userID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /api/sessions → %d", resp.StatusCode)
	}
	var out struct {
		Sessions []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.Sessions
}

func patchJSON(t *testing.T, srv *httptest.Server, path, body string) {
	t.Helper()
	req, _ := http.NewRequest("PATCH", srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("PATCH %s → %d", path, resp.StatusCode)
	}
}

func mustCreateSessionViaAPI(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	resp, err := http.Post(srv.URL+"/api/sessions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if _, err := uuid.Parse(out.ID); err != nil {
		t.Fatalf("session id %q not uuid", out.ID)
	}
	return out.ID
}

func postJSON(t *testing.T, srv *httptest.Server, path, body string) struct {
	Seq     int64 `json:"seq"`
	Deduped bool  `json:"deduped"`
} {
	t.Helper()
	resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("POST %s → %d", path, resp.StatusCode)
	}
	var out struct {
		Seq     int64 `json:"seq"`
		Deduped bool  `json:"deduped"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func getMessages(t *testing.T, srv *httptest.Server, sid string, afterSeq int64) []struct {
	Seq  int64           `json:"seq"`
	Kind string          `json:"kind"`
	Body json.RawMessage `json:"content"`
} {
	t.Helper()
	resp, err := http.Get(srv.URL + "/api/sessions/" + sid + "/messages?after_seq=" + strconv.FormatInt(afterSeq, 10))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Messages []struct {
			Seq  int64           `json:"seq"`
			Kind string          `json:"kind"`
			Body json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.Messages
}
