// Package api ys-server 的 HTTP API（stdlib net/http，SPA 调试界面用）。
// 实现 hit-send 协议（讲座 15）：客户端生成 msg_id → 去重 → 入队 → 锁/打断 → 起 turn。
// 回显 = SSE 推空信号（changed:{session}）+ GET ?after_seq= 回源拉取（讲座 15 Watching）。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"ys-sandbox/internal/store/pg"
	"ys-sandbox/internal/store/redis"
)

// TurnStarter 把"启动/取消 turn"的 Temporal 依赖收窄成两个动作，测试可假。
type TurnStarter interface {
	StartTurn(ctx context.Context, workflowID, sessionID string) error
	CancelTurn(ctx context.Context, workflowID string) error
}

type Server struct {
	pg      *pg.PG
	redis   *redis.Redis
	starter TurnStarter
	userID  string // v1 常量 "local"
	mux     *http.ServeMux
}

func New(p *pg.PG, r *redis.Redis, s TurnStarter, userID string) *Server {
	srv := &Server{pg: p, redis: r, starter: s, userID: userID, mux: http.NewServeMux()}
	srv.mux.HandleFunc("POST /api/sessions", srv.createSession)
	srv.mux.HandleFunc("GET /api/sessions", srv.listSessions)
	srv.mux.HandleFunc("GET /api/sessions/{id}", srv.getSession)
	srv.mux.HandleFunc("PATCH /api/sessions/{id}", srv.renameSession)
	srv.mux.HandleFunc("DELETE /api/sessions/{id}", srv.deleteSession)
	srv.mux.HandleFunc("POST /api/sessions/{id}/messages", srv.sendMessage)
	srv.mux.HandleFunc("GET /api/sessions/{id}/messages", srv.listMessages)
	srv.mux.HandleFunc("GET /api/sessions/{id}/events", srv.events)
	return srv
}

func (s *Server) Handler() http.Handler { return s.mux }

// WithStatic 挂载内嵌 SPA（GET / → dist/）。coder go:embed 同款做法。
func (s *Server) WithStatic(fsys fs.FS) *Server {
	s.mux.Handle("GET /", http.FileServerFS(fsys))
	return s
}

type createSessionRequest struct {
	UserID string `json:"user_id"` // 空则用 server 默认值
	Title  string `json:"title"`
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	var req createSessionRequest
	// 容忍空 body（裸 curl -X POST）：EOF 不算错误
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, err, http.StatusBadRequest)
		return
	}
	userID := req.UserID
	if userID == "" {
		userID = s.userID
	}
	sess, err := s.pg.CreateSession(r.Context(), userID, req.Title)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]string{"id": sess.ID.String(), "user_id": sess.UserID, "title": sess.Title})
}

// getSession SPA 刷新后恢复展示（title/user_id），也用于校验 localStorage 里的 id 是否还有效。
func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	sid, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, fmt.Errorf("bad session id"), http.StatusBadRequest)
		return
	}
	sess, err := s.pg.GetSession(r.Context(), sid)
	if errors.Is(err, pg.ErrNotFound) {
		writeErr(w, fmt.Errorf("session not found"), http.StatusNotFound)
		return
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]string{"id": sess.ID.String(), "user_id": sess.UserID, "title": sess.Title})
}

// listSessions SPA 侧栏会话列表：?user_id= 缺省用 server 默认用户；最近活跃倒序。
func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	userID := r.URL.Query().Get("user_id")
	if userID == "" {
		userID = s.userID
	}
	rows, err := s.pg.ListSessions(r.Context(), userID)
	if err != nil {
		writeErr(w, err)
		return
	}
	type sessDTO struct {
		ID     string `json:"id"`
		UserID string `json:"user_id"`
		Title  string `json:"title"`
	}
	out := make([]sessDTO, 0, len(rows))
	for _, row := range rows {
		out = append(out, sessDTO{ID: row.ID.String(), UserID: row.UserID, Title: row.Title})
	}
	writeJSON(w, map[string]any{"sessions": out})
}

type renameRequest struct {
	Title string `json:"title"`
}

// renameSession SPA 手动改名（也供首条消息自动命名复用 store 层）。
func (s *Server) renameSession(w http.ResponseWriter, r *http.Request) {
	sid, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, fmt.Errorf("bad session id"), http.StatusBadRequest)
		return
	}
	var req renameRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, err, http.StatusBadRequest)
		return
	}
	sess, err := s.pg.RenameSession(r.Context(), sid, req.Title)
	if errors.Is(err, pg.ErrNotFound) {
		writeErr(w, fmt.Errorf("session not found"), http.StatusNotFound)
		return
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]string{"id": sess.ID.String(), "user_id": sess.UserID, "title": sess.Title})
}

// deleteSession 删会话连同消息历史（SPA 侧栏删除）。
func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	sid, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, fmt.Errorf("bad session id"), http.StatusBadRequest)
		return
	}
	if err := s.pg.DeleteSession(r.Context(), sid); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]string{"id": sid.String()})
}

// autoTitle 首条消息自动命名：仅在标题仍为空时生效（显式标题/手动 rename 不被覆盖）。
// 失败只记日志级忽略——命名是锦上添花，不能影响发消息主链路。
func (s *Server) autoTitle(ctx context.Context, sid uuid.UUID, text string) {
	sess, err := s.pg.GetSession(ctx, sid)
	if err != nil || sess.Title != "" {
		return
	}
	runes := []rune(strings.TrimSpace(text))
	title := string(runes)
	if len(runes) > 30 {
		title = string(runes[:30]) + "…"
	}
	_, _ = s.pg.RenameSession(ctx, sid, title)
}

type sendRequest struct {
	MsgID string `json:"msg_id"`
	Text  string `json:"text"`
}

// sendMessage 讲座 15 Sending + 讲座 17 用户特权的完整实现。
// 顺序铁律：先落 PG（幂等）→ 再入队 → 再处理锁（队列可丢，消息不丢）。
func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sid, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, fmt.Errorf("bad session id"), http.StatusBadRequest)
		return
	}
	var req sendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, err, http.StatusBadRequest)
		return
	}
	msgID, err := uuid.Parse(req.MsgID)
	if err != nil || req.Text == "" {
		writeErr(w, fmt.Errorf("msg_id 必须是 uuid 且 text 非空"), http.StatusBadRequest)
		return
	}

	content, _ := json.Marshal(map[string]string{"text": req.Text})
	seq, inserted, err := s.pg.InsertMessage(ctx, pg.Message{
		SessionID: sid, Kind: pg.KindUser, Content: content, MsgID: &msgID,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	if !inserted {
		// 客户端重发：直接返回已存在的 seq，不再入队/触发（讲座 15 去重）
		writeJSON(w, map[string]any{"seq": seq, "deduped": true})
		return
	}

	// 首条消息自动命名：会话还没标题时取消息开头当标题（ChatGPT 同款，可再手动 rename）
	if seq == 1 {
		s.autoTitle(ctx, sid, req.Text)
	}

	if err := s.redis.PushMessage(ctx, redis.Queue{SessionID: sid.String()}, strconv.FormatInt(seq, 10)); err != nil {
		writeErr(w, err)
		return
	}

	workflowID := fmt.Sprintf("turn:%s:%s", sid, uuid.NewString())
	ok, err := s.redis.TryAcquireTurn(ctx, sid.String(), workflowID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !ok {
		// 有 turn 在跑 → 用户特权：打断它，合并队列开新 turn（讲座 17）
		holder, err := s.redis.TurnHolder(ctx, sid.String())
		if err != nil {
			writeErr(w, err)
			return
		}
		if holder != "" {
			if err := s.starter.CancelTurn(ctx, holder); err != nil {
				writeErr(w, fmt.Errorf("cancel running turn: %w", err))
				return
			}
		}
		if err := s.redis.ForceSetTurn(ctx, sid.String(), workflowID); err != nil {
			writeErr(w, err)
			return
		}
	}
	if err := s.starter.StartTurn(ctx, workflowID, sid.String()); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"seq": seq, "deduped": false})
}

func (s *Server) listMessages(w http.ResponseWriter, r *http.Request) {
	sid, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, fmt.Errorf("bad session id"), http.StatusBadRequest)
		return
	}
	afterSeq, _ := strconv.ParseInt(r.URL.Query().Get("after_seq"), 10, 64)
	msgs, err := s.pg.ListMessages(r.Context(), sid, afterSeq)
	if err != nil {
		writeErr(w, err)
		return
	}
	type msgDTO struct {
		Seq     int64           `json:"seq"`
		Kind    string          `json:"kind"`
		Content json.RawMessage `json:"content"`
		TurnID  string          `json:"turn_id,omitempty"`
	}
	out := make([]msgDTO, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, msgDTO{Seq: m.Seq, Kind: m.Kind, Content: m.Content, TurnID: m.TurnID})
	}
	writeJSON(w, map[string]any{"messages": out})
}

// events SSE 桥：订阅 Redis 空信号，原样转发给 SPA（讲座 15 Watching：丢了无害）。
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("id")
	if _, err := uuid.Parse(sid); err != nil {
		writeErr(w, fmt.Errorf("bad session id"), http.StatusBadRequest)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, errors.New("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch, unsubscribe := s.redis.SubscribeChanged(r.Context(), sid)
	defer unsubscribe()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			fmt.Fprint(w, "data: changed\n\n")
			flusher.Flush()
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error, code ...int) {
	status := http.StatusInternalServerError
	if len(code) > 0 {
		status = code[0]
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
