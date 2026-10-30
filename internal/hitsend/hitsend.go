// Package hitsend 是讲座 15 hit-send 协议的唯一归属：
// msg_id 客户端生成 → 幂等落 PG → 入队 → 锁/打断 → 起 turn。
// 顺序铁律与打断决策全部在本包；HTTP（api 包）只是它的一个入口适配器，
// Temporal（workflow 包）只是 Starter 接口的一个实现。
package hitsend

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"ys-sandbox/internal/store/pg"
	"ys-sandbox/internal/store/redis"
)

// Starter 把"启动/取消 turn"的 Temporal 依赖收窄成两个动作（workflow.TemporalStarter 实现；测试可假）。
type Starter interface {
	StartTurn(ctx context.Context, workflowID, sessionID string) error
	CancelTurn(ctx context.Context, workflowID string) error
}

// Service hit-send 协议入口。锁的释放在对侧：workflow TurnWorkflow 的 defer ReleaseTurn
// （workflow 里碰 Redis 必须走 activity，所以释放没法收进本包——两边注释互链）。
type Service struct {
	PG      *pg.PG
	Redis   *redis.Redis
	Starter Starter
}

// Submit 一条用户消息进会话。
// 顺序铁律：先落 PG（幂等）→ 再入队 → 再处理锁（队列可丢，消息不丢）。
// 返回 deduped=true 表示客户端重发：直接返回已存在的 seq，不再入队/触发（讲座 15 去重）。
func (s *Service) Submit(ctx context.Context, sessionID, msgID uuid.UUID, text string) (seq int64, deduped bool, err error) {
	seq, inserted, err := s.PG.InsertMessage(ctx, pg.Message{
		SessionID: sessionID, Kind: pg.KindUser, Content: pg.MarshalText(text), MsgID: &msgID,
	})
	if err != nil {
		return 0, false, err
	}
	// M4 续期：用户发消息 = 会话活跃（含 deduped 重发——用户确实按了发送）。
	// 失败只记日志级忽略：续期是优化，不能挡住发消息主链路。
	_ = s.PG.RefreshSandboxActivityBySession(ctx, sessionID)
	if !inserted {
		return seq, true, nil
	}

	// 首条消息自动命名：会话还没标题时取消息开头当标题（ChatGPT 同款，可再手动 rename）
	if seq == 1 {
		s.autoTitle(ctx, sessionID, text)
	}

	sid := sessionID.String()
	if err := s.Redis.PushMessage(ctx, redis.Queue{SessionID: sid}, strconv.FormatInt(seq, 10)); err != nil {
		return 0, false, err
	}

	workflowID := fmt.Sprintf("turn:%s:%s", sid, uuid.NewString())
	ok, err := s.Redis.TryAcquireTurn(ctx, sid, workflowID)
	if err != nil {
		return 0, false, err
	}
	if !ok {
		// 有 turn 在跑 → 用户特权：打断它，合并队列开新 turn（讲座 17）
		holder, err := s.Redis.TurnHolder(ctx, sid)
		if err != nil {
			return 0, false, err
		}
		if holder != "" {
			if err := s.Starter.CancelTurn(ctx, holder); err != nil {
				return 0, false, fmt.Errorf("cancel running turn: %w", err)
			}
		}
		if err := s.Redis.ForceSetTurn(ctx, sid, workflowID); err != nil {
			return 0, false, err
		}
	}
	if err := s.Starter.StartTurn(ctx, workflowID, sid); err != nil {
		return 0, false, err
	}
	return seq, false, nil
}

// autoTitle 仅在标题仍为空时生效（显式标题/手动 rename 不被覆盖）。
// 失败忽略——命名是锦上添花，不能影响发消息主链路。
func (s *Service) autoTitle(ctx context.Context, sid uuid.UUID, text string) {
	sess, err := s.PG.GetSession(ctx, sid)
	if err != nil || sess.Title != "" {
		return
	}
	runes := []rune(strings.TrimSpace(text))
	title := string(runes)
	if len(runes) > 30 {
		title = string(runes[:30]) + "…"
	}
	_, _ = s.PG.RenameSession(ctx, sid, title)
}
