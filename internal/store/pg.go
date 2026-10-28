// Package store 事实源与协调态的存储层。
// 红线：事实源只写 PG/文件存储；Redis 只放协调态（队列/锁/信号）。
// 对应 docs/architecture/data-model.md。
package store

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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
}

func NewPG(ctx context.Context, dsn string) (*PG, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &PG{pool: pool}, nil
}

func (p *PG) Close() { p.pool.Close() }

// ApplySchema 建表（幂等）。启动时调用。
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
	s := Session{ID: id, UserID: userID, Title: title}
	err = p.pool.QueryRow(ctx,
		`INSERT INTO sessions (id, user_id, title) VALUES ($1, $2, $3) RETURNING created_at`,
		id, userID, title).Scan(&s.CreatedAt)
	return s, err
}

// InsertMessage 幂等插入一条消息。
// msg_id 冲突（客户端重发）→ inserted=false，返回已存在行的 seq（讲座 15 去重）。
// seq 在同一会话内递增；用 advisory xact lock 防并发插入撞 PK。
func (p *PG) InsertMessage(ctx context.Context, m Message) (seq int64, inserted bool, err error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1::text))`, m.SessionID); err != nil {
		return 0, false, err
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO messages (session_id, seq, kind, content, msg_id, turn_id)
		VALUES ($1, (SELECT COALESCE(MAX(seq), 0) + 1 FROM messages WHERE session_id = $1), $2, $3, $4, NULLIF($5, ''))
		ON CONFLICT (msg_id) DO NOTHING
		RETURNING seq`,
		m.SessionID, m.Kind, m.Content, m.MsgID, m.TurnID).Scan(&seq)
	if errors.Is(err, pgx.ErrNoRows) {
		// msg_id 冲突：回查已存在行的 seq
		if m.MsgID == nil {
			return 0, false, errors.New("no row inserted but msg_id is nil")
		}
		err = tx.QueryRow(ctx,
			`SELECT seq FROM messages WHERE msg_id = $1`, *m.MsgID).Scan(&seq)
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
	rows, err := p.pool.Query(ctx, `
		SELECT session_id, seq, kind, content, msg_id, COALESCE(turn_id, ''), created_at
		FROM messages WHERE session_id = $1 AND seq > $2 ORDER BY seq`,
		sessionID, afterSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.SessionID, &m.Seq, &m.Kind, &m.Content, &m.MsgID, &m.TurnID, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
