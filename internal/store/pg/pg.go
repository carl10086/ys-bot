// Package pg 业务 PG 事实源（sessions/messages）。
// SQL 在 queries.sql（review 对象），类型安全的调用代码由 sqlc 生成到 gen/（不手改）；
// 本文件只做薄门面：事务编排、错误翻译、gen 类型不外漏。
// 对应 docs/architecture/data-model.md。
package pg

import (
	"context"
	"crypto/md5"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"ys-bot/internal/store/pg/gen"
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
	if _, err := p.pool.Exec(ctx, "DROP TABLE IF EXISTS messages, sessions, sandboxes, memories, skills, memory_ops"); err != nil {
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

// ErrMemoryConflict 表示记忆在提取期间被其他写入改变，旧快照的 ops 不得提交。
var ErrMemoryConflict = errors.New("pg: memory snapshot conflict")

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

// RefreshSandboxActivity 续期写入点（ExecTool）：有任何活动就把 last_activity_at 刷成 now()。
func (p *PG) RefreshSandboxActivity(ctx context.Context, id uuid.UUID) error {
	return p.q.RefreshSandboxActivity(ctx, id)
}

// RefreshSandboxActivityBySession 续期写入点（hitsend.Submit）：session→user→sandbox 一跳完成。
func (p *PG) RefreshSandboxActivityBySession(ctx context.Context, sessionID uuid.UUID) error {
	return p.q.RefreshSandboxActivityBySession(ctx, sessionID)
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

// ===== M5 记忆与技能（spec: 2026-11-02-m5-memory-design.md；选型 ADR-0002）=====

// 记忆类型。"记忆是信息不是指令"（讲座 30）：事实在这里，流程在 skills 表。
const (
	MemoryTypeProfile = "profile"
	MemoryTypeLog     = "log"
	MemoryTypeNote    = "note"
)

type Memory struct {
	ID         uuid.UUID
	UserID     string
	Type       string
	Text       string
	Importance int32
	Hash       string
	ExpiresAt  *time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

type Skill struct {
	ID          uuid.UUID
	UserID      string
	Name        string
	Description string
	Content     string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// SkillCatalogEntry 注入清单项：只有 name+description，全文靠 memory_read 按需读。
type SkillCatalogEntry struct {
	Name        string
	Description string
}

// SearchHit memory_search 的命中行（记忆或技能）。
type SearchHit struct {
	ID   uuid.UUID
	Kind string // memory / skill
	Text string
}

func memoryRow(m gen.Memory) Memory {
	out := Memory{
		ID: m.ID, UserID: m.UserID, Type: m.Type, Text: m.Text,
		Importance: m.Importance, Hash: m.Hash,
		CreatedAt: m.CreatedAt.Time, UpdatedAt: m.UpdatedAt.Time,
	}
	if m.ExpiresAt.Valid {
		t := m.ExpiresAt.Time
		out.ExpiresAt = &t
	}
	return out
}

func memoryRows(rows []gen.Memory) []Memory {
	out := make([]Memory, 0, len(rows))
	for _, r := range rows {
		out = append(out, memoryRow(r))
	}
	return out
}

func (p *PG) ListProfileMemories(ctx context.Context, userID string) ([]Memory, error) {
	rows, err := p.q.ListProfileMemories(ctx, userID)
	return memoryRows(rows), err
}

func (p *PG) ListRecentLogMemories(ctx context.Context, userID string) ([]Memory, error) {
	rows, err := p.q.ListRecentLogMemories(ctx, userID)
	return memoryRows(rows), err
}

func (p *PG) ListActiveNotes(ctx context.Context, userID string) ([]Memory, error) {
	rows, err := p.q.ListActiveNotes(ctx, userID)
	return memoryRows(rows), err
}

// ListAllMemoriesForUser 提取上下文：全部未消退记忆（过期 note 不给提取模型看——"只能删见过的"）。
func (p *PG) ListAllMemoriesForUser(ctx context.Context, userID string) ([]Memory, error) {
	rows, err := p.q.ListAllMemoriesForUser(ctx, userID)
	return memoryRows(rows), err
}

func (p *PG) GetMemoryByID(ctx context.Context, id uuid.UUID) (Memory, error) {
	r, err := p.q.GetMemoryByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Memory{}, ErrNotFound
	}
	if err != nil {
		return Memory{}, err
	}
	return memoryRow(r), nil
}

func (p *PG) ListSkillCatalog(ctx context.Context, userID string) ([]SkillCatalogEntry, error) {
	rows, err := p.q.ListSkillCatalog(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]SkillCatalogEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, SkillCatalogEntry{Name: r.Name, Description: r.Description})
	}
	return out, nil
}

func (p *PG) GetSkillByName(ctx context.Context, userID, name string) (Skill, error) {
	r, err := p.q.GetSkillByName(ctx, gen.GetSkillByNameParams{UserID: userID, Name: name})
	if errors.Is(err, pgx.ErrNoRows) {
		return Skill{}, ErrNotFound
	}
	if err != nil {
		return Skill{}, err
	}
	return Skill{ID: r.ID, UserID: r.UserID, Name: r.Name, Description: r.Description,
		Content: r.Content, CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time}, nil
}

// SearchMemories memory_search 工具后端（v1 关键词 ILIKE；向量检索留作演进，ADR-0002）。
func (p *PG) SearchMemories(ctx context.Context, userID, query string, limit int32) ([]SearchHit, error) {
	rows, err := p.q.SearchMemories(ctx, gen.SearchMemoriesParams{UserID: userID, Column2: pgtype.Text{String: query, Valid: true}, Limit: limit})
	if err != nil {
		return nil, err
	}
	out := make([]SearchHit, 0, len(rows))
	for _, r := range rows {
		out = append(out, SearchHit{ID: r.ID, Kind: r.Kind, Text: r.Text})
	}
	return out, nil
}

// ListTurnMessages pass 的提取原料：本轮消息。turn_id 仅 assistant 行有，用户消息靠 batch seq 兜。
func (p *PG) ListTurnMessages(ctx context.Context, sessionID uuid.UUID, turnID string, batchSeqs []int64) ([]Message, error) {
	rows, err := p.q.ListTurnMessages(ctx, gen.ListTurnMessagesParams{
		SessionID: sessionID,
		TurnID:    pgtype.Text{String: turnID, Valid: turnID != ""},
		Column3:   batchSeqs,
	})
	if err != nil {
		return nil, err
	}
	out := make([]Message, 0, len(rows))
	for _, r := range rows {
		out = append(out, Message{
			SessionID: r.SessionID, Seq: r.Seq, Kind: r.Kind, Content: r.Content,
			MsgID: r.MsgID, TurnID: r.TurnID.String, CreatedAt: r.CreatedAt.Time,
		})
	}
	return out, nil
}

// MemoryOp 一次记忆写操作（memory pass 产出 / agent memory_save 直写）。
type MemoryOp struct {
	Op          string    // add / update / delete
	TargetType  string    // memory / skill
	TargetID    uuid.UUID // update/delete memory 的目标
	Type        string    // add memory：profile / log / note
	Text        string
	Importance  int32
	TTLMinutes  *int32 // note 的消退时长（转成 expires_at）
	Name        string // skill
	Description string // skill
	Content     string // skill 全文
}

// GetMemoryVersion 返回当前用户的记忆写入版本，供 pass 快照校验。
func (p *PG) GetMemoryVersion(ctx context.Context, userID string) (int64, error) {
	return p.q.GetMemoryVersion(ctx, userID)
}

// ApplyMemoryOps 事务内逐条应用 ops，每条配一行 memory_ops 审计（成对出现，spec Boundary）。
// 宽容 LLM 产出：update/delete 目标不存在 = 跳过；add 撞 hash = 去重跳过不审计。
func (p *PG) ApplyMemoryOps(ctx context.Context, userID string, ops []MemoryOp, source string) (int, error) {
	return p.applyMemoryOps(ctx, userID, ops, source, nil)
}

// ApplyMemoryOpsAtVersion 仅当记忆仍停留在 expectedVersion 时提交，供 memory pass 防止旧快照覆盖新写入。
func (p *PG) ApplyMemoryOpsAtVersion(ctx context.Context, userID string, ops []MemoryOp, source string, expectedVersion int64) (int, error) {
	return p.applyMemoryOps(ctx, userID, ops, source, &expectedVersion)
}

func (p *PG) applyMemoryOps(ctx context.Context, userID string, ops []MemoryOp, source string, expectedVersion *int64) (int, error) {
	// per-user 写锁串行化 pass 与 agent 工具；pass 的读取/提取顺序由队列保证。
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	// per-user 事务级 advisory lock（commit/rollback 自动释放）
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", userID); err != nil {
		return 0, fmt.Errorf("memory pass lock: %w", err)
	}
	q := p.q.WithTx(tx)
	if expectedVersion != nil {
		// Apply 成功响应可能丢失；先识别本 pass 已提交，避免把自己的版本变化误判为外部冲突。
		committed, err := q.CountMemoryOpsBySource(ctx, gen.CountMemoryOpsBySourceParams{UserID: userID, Source: source})
		if err != nil {
			return 0, err
		}
		if committed > 0 {
			return int(committed), nil
		}
		current, err := q.GetMemoryVersion(ctx, userID)
		if err != nil {
			return 0, err
		}
		if current != *expectedVersion {
			return 0, ErrMemoryConflict
		}
	}

	audit := func(op MemoryOp, targetID *uuid.UUID, before, after string) error {
		return q.InsertMemoryOp(ctx, gen.InsertMemoryOpParams{
			UserID:     userID,
			Op:         op.Op,
			TargetType: op.TargetType,
			TargetID:   targetID,
			BeforeText: pgtype.Text{String: before, Valid: before != ""},
			AfterText:  pgtype.Text{String: after, Valid: after != ""},
			Source:     source,
		})
	}

	applied := 0
	for _, op := range ops {
		switch {
		case op.Op == "add" && op.TargetType == "memory":
			id, err := uuid.NewV7()
			if err != nil {
				return applied, fmt.Errorf("uuidv7: %w", err)
			}
			expires := pgtype.Timestamptz{}
			if op.TTLMinutes != nil {
				expires = pgtype.Timestamptz{Time: time.Now().Add(time.Duration(*op.TTLMinutes) * time.Minute), Valid: true}
			}
			sum := md5.Sum([]byte(op.Text))
			n, err := q.AddMemory(ctx, gen.AddMemoryParams{
				ID: id, UserID: userID, Type: op.Type, Text: op.Text,
				Importance: op.Importance, Hash: hex.EncodeToString(sum[:]), ExpiresAt: expires,
			})
			if err != nil {
				return applied, err
			}
			if n == 0 {
				continue // hash 去重：无事发生，不审计
			}
			if err := audit(op, &id, "", op.Text); err != nil {
				return applied, err
			}
			applied++
		case op.Op == "update" && op.TargetType == "memory":
			old, err := q.GetMemoryByID(ctx, op.TargetID)
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return applied, err
			}
			expires := old.ExpiresAt
			if op.TTLMinutes != nil {
				expires = pgtype.Timestamptz{Time: time.Now().Add(time.Duration(*op.TTLMinutes) * time.Minute), Valid: true}
			}
			sum := md5.Sum([]byte(op.Text))
			hash := hex.EncodeToString(sum[:])
			existing, err := q.GetMemoryByHash(ctx, gen.GetMemoryByHashParams{UserID: userID, Hash: hash})
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return applied, err
			}
			if err == nil && existing.ID != op.TargetID {
				// update 保留 target 身份：删除同正文的冲突行，再完整更新 target。
				// 隐式删除也写审计，避免无声丢失另一条记忆。
				if _, err := q.DeleteMemory(ctx, existing.ID); err != nil {
					return applied, err
				}
				if err := audit(MemoryOp{Op: "delete", TargetType: "memory"}, &existing.ID, existing.Text, ""); err != nil {
					return applied, err
				}
				applied++
			}
			n, err := q.UpdateMemory(ctx, gen.UpdateMemoryParams{
				ID: op.TargetID, Text: op.Text, Importance: op.Importance, ExpiresAt: expires,
				Hash: hash,
			})
			if err != nil {
				return applied, err
			}
			if n == 0 {
				continue
			}
			if err := audit(op, &op.TargetID, old.Text, op.Text); err != nil {
				return applied, err
			}
			applied++
		case op.Op == "delete" && op.TargetType == "memory":
			old, err := q.GetMemoryByID(ctx, op.TargetID)
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return applied, err
			}
			if _, err := q.DeleteMemory(ctx, op.TargetID); err != nil {
				return applied, err
			}
			if err := audit(op, &op.TargetID, old.Text, ""); err != nil {
				return applied, err
			}
			applied++
		case (op.Op == "add" || op.Op == "update") && op.TargetType == "skill":
			var before string
			old, err := q.GetSkillByName(ctx, gen.GetSkillByNameParams{UserID: userID, Name: op.Name})
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return applied, err // 真错误不能吞——否则审计 before 会记成空串（review S1）
			}
			if err == nil {
				before = old.Content
			}
			id, err := uuid.NewV7()
			if err != nil {
				return applied, fmt.Errorf("uuidv7: %w", err)
			}
			realID, err := q.UpsertSkill(ctx, gen.UpsertSkillParams{
				ID: id, UserID: userID, Name: op.Name, Description: op.Description, Content: op.Content,
			})
			if err != nil {
				return applied, err
			}
			if err := audit(op, &realID, before, op.Content); err != nil {
				return applied, err
			}
			applied++
		case op.Op == "delete" && op.TargetType == "skill":
			old, err := q.GetSkillByName(ctx, gen.GetSkillByNameParams{UserID: userID, Name: op.Name})
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return applied, err
			}
			if _, err := q.DeleteSkill(ctx, gen.DeleteSkillParams{UserID: userID, Name: op.Name}); err != nil {
				return applied, err
			}
			if err := audit(op, &old.ID, old.Content, ""); err != nil {
				return applied, err
			}
			applied++
		default:
			// 未知 op/类型组合：跳过（对 LLM 产出宽容）
		}
	}
	return applied, tx.Commit(ctx)
}

// GetSkillByID memory_read 工具用：按 id 读技能全文。
func (p *PG) GetSkillByID(ctx context.Context, id uuid.UUID) (Skill, error) {
	r, err := p.q.GetSkillByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Skill{}, ErrNotFound
	}
	if err != nil {
		return Skill{}, err
	}
	return Skill{ID: r.ID, UserID: r.UserID, Name: r.Name, Description: r.Description,
		Content: r.Content, CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time}, nil
}
