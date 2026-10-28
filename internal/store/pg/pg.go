// Package pg 业务 PG 事实源（sessions/messages）。
// SQL 在 queries.sql（review 对象），类型安全的调用代码由 sqlc 生成到 gen/（不手改）；
// 本文件只做薄门面：事务编排、错误翻译、gen 类型不外漏。
// 对应 docs/architecture/data-model.md。
package pg

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"ys-sandbox/internal/store/pg/gen"
)

//go:embed schema.sql
var schemaSQL string

// 消息种类（data-model.md：kind ∈ user / assistant / tool_call / tool_result）。
const (
	KindUser       = "user"
	KindAssistant  = "assistant"
	KindToolCall   = "tool_call"
	KindToolResult = "tool_result"
)

type Session struct {
	ID        uuid.UUID
	UserID    string
	Title     string
	CreatedAt time.Time
}

type Message struct {
	SessionID uuid.UUID
	Seq       int64
	Kind      string
	Content   []byte // jsonb 原文
	MsgID     *uuid.UUID
	TurnID    string
	CreatedAt time.Time
}

type PG struct {
	pool *pgxpool.Pool
	q    *gen.Queries
}

func New(ctx context.Context, dsn string) (*PG, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &PG{pool: pool, q: gen.New(pool)}, nil
}

func (p *PG) Close() { p.pool.Close() }

// ApplySchema 建表（幂等）。启动时调用——不引入 migrate 工具（选型定稿 #9）。
func (p *PG) ApplySchema(ctx context.Context) error {
	_, err := p.pool.Exec(ctx, schemaSQL)
	return err
}

// ResetSchema 仅测试用：drop 后重建。
func (p *PG) ResetSchema(ctx context.Context) error {
	if _, err := p.pool.Exec(ctx, "DROP TABLE IF EXISTS messages, sessions"); err != nil {
		return err
	}
	return p.ApplySchema(ctx)
}

func (p *PG) CreateSession(ctx context.Context, userID, title string) (Session, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return Session{}, fmt.Errorf("uuidv7: %w", err)
	}
	s, err := p.q.CreateSession(ctx, gen.CreateSessionParams{ID: id, UserID: userID, Title: title})
	if err != nil {
		return Session{}, err
	}
	return Session{ID: s.ID, UserID: s.UserID, Title: s.Title, CreatedAt: s.CreatedAt.Time}, nil
}

// InsertMessage 幂等插入一条消息。
// msg_id 冲突（客户端重发）→ inserted=false，返回已存在行的 seq（讲座 15 去重）。
// seq 在同一会话内递增；事务 + advisory lock 防并发撞 PK。
func (p *PG) InsertMessage(ctx context.Context, m Message) (seq int64, inserted bool, err error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback(ctx)
	q := p.q.WithTx(tx)

	if err := q.LockSessionForSeq(ctx, m.SessionID.String()); err != nil {
		return 0, false, err
	}
	seq, err = q.InsertMessageDedup(ctx, gen.InsertMessageDedupParams{
		SessionID: m.SessionID,
		Kind:      m.Kind,
		Content:   m.Content,
		MsgID:     m.MsgID,
		TurnID:    pgtype.Text{String: m.TurnID, Valid: m.TurnID != ""},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// msg_id 冲突：回查已存在行的 seq
		if m.MsgID == nil {
			return 0, false, errors.New("no row inserted but msg_id is nil")
		}
		seq, err = q.GetMessageSeqByMsgID(ctx, m.MsgID)
		if err != nil {
			return 0, false, err
		}
		return seq, false, tx.Commit(ctx)
	}
	if err != nil {
		return 0, false, err
	}
	return seq, true, tx.Commit(ctx)
}

// ListMessages 返回会话中 seq > afterSeq 的全部消息，按 seq 升序（SPA 回显协议）。
func (p *PG) ListMessages(ctx context.Context, sessionID uuid.UUID, afterSeq int64) ([]Message, error) {
	rows, err := p.q.ListMessagesAfter(ctx, gen.ListMessagesAfterParams{SessionID: sessionID, Seq: afterSeq})
	if err != nil {
		return nil, err
	}
	out := make([]Message, 0, len(rows))
	for _, r := range rows {
		out = append(out, Message{
			SessionID: r.SessionID,
			Seq:       r.Seq,
			Kind:      r.Kind,
			Content:   r.Content,
			MsgID:     r.MsgID,
			TurnID:    r.TurnID.String,
			CreatedAt: r.CreatedAt.Time,
		})
	}
	return out, nil
}
