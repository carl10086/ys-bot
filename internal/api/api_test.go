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

	// 触发一次真实变更信号（发布者是 workflow/worker，这里直接发到同一频道）
	time.Sleep(50 * time.Millisecond) // 等订阅建立
	if err := rdb.PublishChanged(context.Background(), sid); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 128)
	done := make(chan string, 1)
	go func() {
		n, _ := resp.Body.Read(buf)
		done <- string(buf[:n])
	}()
	select {
	case got := <-done:
		if !strings.HasPrefix(got, "data:") {
			t.Fatalf("want SSE data frame, got %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("3s 内没收到 SSE 信号")
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

