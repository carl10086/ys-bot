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
	ID        uuid.UUID // uuidv7，应用生成
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
	if _, err := p.pool.Exec(ctx, "DROP TABLE IF EXISTS messages, sessions, sandboxes"); err != nil {
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

// ErrNotFound 记录不存在（翻译 pgx.ErrNoRows，调用方不用 import pgx）。
var ErrNotFound = errors.New("pg: not found")

// SessionSummary 会话列表项：会话本身 + 最近活跃时间（列表排序键）。
type SessionSummary struct {
	Session
	LastActiveAt time.Time
}

// ListSessions 按 user 列会话，最近活跃倒序（SPA 会话列表）。
func (p *PG) ListSessions(ctx context.Context, userID string) ([]SessionSummary, error) {
	rows, err := p.q.ListSessionsByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]SessionSummary, 0, len(rows))
	for _, r := range rows {
		out = append(out, SessionSummary{
			Session:      Session{ID: r.ID, UserID: r.UserID, Title: r.Title, CreatedAt: r.CreatedAt.Time},
			LastActiveAt: r.LastActiveAt.Time,
		})
	}
	return out, nil
}

// RenameSession 改会话标题（SPA rename）。
func (p *PG) RenameSession(ctx context.Context, id uuid.UUID, title string) (Session, error) {
	s, err := p.q.UpdateSessionTitle(ctx, gen.UpdateSessionTitleParams{ID: id, Title: title})
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, err
	}
	return Session{ID: s.ID, UserID: s.UserID, Title: s.Title, CreatedAt: s.CreatedAt.Time}, nil
}

// DeleteSession 删会话及其全部消息（messages 有 FK，必须先清；一个事务保证不同时失败）。
func (p *PG) DeleteSession(ctx context.Context, id uuid.UUID) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := p.q.WithTx(tx)
	if err := q.DeleteSessionMessages(ctx, id); err != nil {
		return err
	}
	if err := q.DeleteSession(ctx, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// GetSession 按 id 取会话（SPA 刷新后恢复展示用）。
func (p *PG) GetSession(ctx context.Context, id uuid.UUID) (Session, error) {
	s, err := p.q.GetSession(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, err
	}
	return Session{ID: s.ID, UserID: s.UserID, Title: s.Title, CreatedAt: s.CreatedAt.Time}, nil
}

// InsertMessage 幂等插入一条消息。
// msg_id 已存在（客户端重发）→ inserted=false，返回已存在行的 seq（讲座 15 去重）。
// seq 由 sessions.last_seq 父行计数器发放：行锁串行化同会话插入，回滚不留空号。
// 罕见竞态（两个并发同 msg_id 都过了预检）由 ON CONFLICT 兜底，会烧掉一个 seq——无害（after_seq 是 > 比较）。
func (p *PG) InsertMessage(ctx context.Context, m Message) (seq int64, inserted bool, err error) {
	if m.MsgID != nil {
		// 去重预检：命中直接返回，不烧 seq
		if seq, err := p.q.GetMessageSeqByMsgID(ctx, m.MsgID); err == nil {
			return seq, false, nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return 0, false, err
		}
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback(ctx)
	q := p.q.WithTx(tx)

	seq, err = q.NextSeq(ctx, m.SessionID)
	if err != nil {
		return 0, false, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return 0, false, fmt.Errorf("uuidv7: %w", err)
	}
	_, err = q.InsertMessage(ctx, gen.InsertMessageParams{
		ID:        id,
		SessionID: m.SessionID,
		Seq:       seq,
		Kind:      m.Kind,
		Content:   m.Content,
		MsgID:     m.MsgID,
		TurnID:    pgtype.Text{String: m.TurnID, Valid: m.TurnID != ""},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// 并发同 msg_id：ON CONFLICT 不返行，回查胜者的 seq
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

// ===== sandboxes（M2；data-model.md 2026-10-29 修订）=====

// sandbox 状态机（M2 用子集：creating → awake → stopped；failed 异常路径；
// M4 加 sleeping：VM 停机但快照在宿主盘上，可 resume）。
const (
	SandboxStatusCreating = "creating"
	SandboxStatusAwake    = "awake"
	SandboxStatusSleeping = "sleeping"
	SandboxStatusStopped  = "stopped"
	SandboxStatusFailed   = "failed"
)

// Sandbox 一用户一台 VM 的档案卡。Slot/VMIP 未分配时为空。
type Sandbox struct {
	ID        uuid.UUID
	UserID    string
	HostID    string
	Status    string
	Slot      *int32
	VMIP      string
	CreatedAt time.Time
}

func (p *PG) CreateSandbox(ctx context.Context, userID, hostID string) (Sandbox, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return Sandbox{}, fmt.Errorf("uuidv7: %w", err)
	}
	r, err := p.q.CreateSandbox(ctx, gen.CreateSandboxParams{ID: id, UserID: userID, HostID: hostID})
	if err != nil {
		return Sandbox{}, err
	}
	return sandboxRow(r.ID, r.UserID, r.HostID, r.Status, r.Slot, r.VmIp, r.CreatedAt), nil
}

func (p *PG) GetSandboxByID(ctx context.Context, id uuid.UUID) (Sandbox, error) {
	r, err := p.q.GetSandboxByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Sandbox{}, ErrNotFound
	}
	if err != nil {
		return Sandbox{}, err
	}
	return sandboxRow(r.ID, r.UserID, r.HostID, r.Status, r.Slot, r.VmIp, r.CreatedAt), nil
}

func (p *PG) GetSandboxByUser(ctx context.Context, userID string) (Sandbox, error) {
	r, err := p.q.GetSandboxByUser(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Sandbox{}, ErrNotFound
	}
	if err != nil {
		return Sandbox{}, err
	}
	return sandboxRow(r.ID, r.UserID, r.HostID, r.Status, r.Slot, r.VmIp, r.CreatedAt), nil
}

func (p *PG) ListUsedSlotsByHost(ctx context.Context, hostID string) ([]int32, error) {
	rows, err := p.q.ListUsedSlotsByHost(ctx, hostID)
	if err != nil {
		return nil, err
	}
	out := make([]int32, 0, len(rows))
	for _, s := range rows {
		out = append(out, s.Int32)
	}
	return out, nil
}

func (p *PG) ListActiveSandboxesByHost(ctx context.Context, hostID string) ([]Sandbox, error) {
	rows, err := p.q.ListActiveSandboxesByHost(ctx, hostID)
	if err != nil {
		return nil, err
	}
	out := make([]Sandbox, 0, len(rows))
	for _, r := range rows {
		out = append(out, sandboxRow(r.ID, r.UserID, r.HostID, r.Status, r.Slot, r.VmIp, r.CreatedAt))
	}
	return out, nil
}

func (p *PG) SetSandboxAwake(ctx context.Context, id uuid.UUID, slot int32, vmIP string) error {
	_, err := p.q.SetSandboxAwake(ctx, gen.SetSandboxAwakeParams{
		ID:   id,
		Slot: pgtype.Int4{Int32: slot, Valid: true},
		VmIp: pgtype.Text{String: vmIP, Valid: true},
	})
	return err
}

func (p *PG) SetSandboxStatus(ctx context.Context, id uuid.UUID, status string) error {
	return p.q.SetSandboxStatus(ctx, gen.SetSandboxStatusParams{ID: id, Status: status})
}

func (p *PG) ReleaseSandbox(ctx context.Context, id uuid.UUID) error {
	return p.q.ReleaseSandbox(ctx, id)
}

// ===== M4 快照休眠 =====

// RefreshSandboxActivity 续期写入点：有任何活动就把 last_activity_at 刷成 now()。
func (p *PG) RefreshSandboxActivity(ctx context.Context, id uuid.UUID) error {
	return p.q.RefreshSandboxActivity(ctx, id)
}

// ListSleepCandidates sweeper 全局扫描：idleMinutes 分钟无活动的 awake sandbox（跨宿主）。
func (p *PG) ListSleepCandidates(ctx context.Context, idleMinutes int32) ([]Sandbox, error) {
	rows, err := p.q.ListSleepCandidates(ctx, idleMinutes)
	if err != nil {
		return nil, err
	}
	out := make([]Sandbox, 0, len(rows))
	for _, r := range rows {
		out = append(out, sandboxRow(r.ID, r.UserID, r.HostID, r.Status, r.Slot, r.VmIp, r.CreatedAt))
	}
	return out, nil
}

// TryStartSleep 休眠入场券（never-mid-turn 的互斥核心）：检查与占坑同一原子 UPDATE。
// false = 刚有活动/已在睡/非 awake → 调用方放弃，下轮再说。
func (p *PG) TryStartSleep(ctx context.Context, id uuid.UUID, idleMinutes int32) (bool, error) {
	n, err := p.q.TryStartSleep(ctx, gen.TryStartSleepParams{ID: id, Column2: idleMinutes})
	return n > 0, err
}

// SetSandboxSnapshotRef 写/清快照引用（ref=nil 清空；NULL = 无快照可恢复）。
func (p *PG) SetSandboxSnapshotRef(ctx context.Context, id uuid.UUID, ref *string) error {
	v := pgtype.Text{}
	if ref != nil {
		v = pgtype.Text{String: *ref, Valid: true}
	}
	return p.q.SetSandboxSnapshotRef(ctx, gen.SetSandboxSnapshotRefParams{ID: id, SnapshotRef: v})
}

// ListSleepingSandboxesByHost reconcile 用：本宿主 sleeping 的 sandbox（快照丢失检测）。
func (p *PG) ListSleepingSandboxesByHost(ctx context.Context, hostID string) ([]Sandbox, error) {
	rows, err := p.q.ListSleepingSandboxesByHost(ctx, hostID)
	if err != nil {
		return nil, err
	}
	out := make([]Sandbox, 0, len(rows))
	for _, r := range rows {
		out = append(out, sandboxRow(r.ID, r.UserID, r.HostID, r.Status, r.Slot, r.VmIp, r.CreatedAt))
	}
	return out, nil
}

func sandboxRow(id uuid.UUID, userID, hostID, status string, slot pgtype.Int4, vmIP pgtype.Text, createdAt pgtype.Timestamptz) Sandbox {
	s := Sandbox{ID: id, UserID: userID, HostID: hostID, Status: status, VMIP: vmIP.String, CreatedAt: createdAt.Time}
	if slot.Valid {
		v := slot.Int32
		s.Slot = &v
	}
	return s
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
