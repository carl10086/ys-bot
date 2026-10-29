package redis

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis 协调态三结构（data-model.md）：
//   session:{id}:queue        LIST   会话队列，turn 开始时 drain 成一批
//   session:{id}:active_turn  STRING SET NX EX 600，值 = workflowID
//   changed:{session}         Pub/Sub "something changed" 空信号
type Redis struct {
	client *redis.Client
}

func NewRedis(addr, password string, db int) (*Redis, error) {
	c := redis.NewClient(&redis.Options{Addr: addr, Password: password, DB: db})
	if err := c.Ping(context.Background()).Err(); err != nil {
		return nil, err
	}
	return &Redis{client: c}, nil
}

func (r *Redis) Close() { r.client.Close() }

// Queue 一个会话的消息队列。payload 是消息在 PG 里的 seq（消息已先落 PG，队列可丢）。
type Queue struct {
	SessionID string
}

func (q Queue) key() string { return fmt.Sprintf("session:%s:queue", q.SessionID) }

func turnKey(sessionID string) string  { return fmt.Sprintf("session:%s:active_turn", sessionID) }
func changedKey(sessionID string) string { return fmt.Sprintf("changed:%s", sessionID) }

func (r *Redis) PushMessage(ctx context.Context, q Queue, payload string) error {
	return r.client.RPush(ctx, q.key(), payload).Err()
}

// DrainQueue 原子地取出并清空队列（MULTI：LRANGE + DEL），返回 FIFO 顺序的一批。
func (r *Redis) DrainQueue(ctx context.Context, q Queue) ([]string, error) {
	pipe := r.client.TxPipeline()
	lrange := pipe.LRange(ctx, q.key(), 0, -1)
	pipe.Del(ctx, q.key())
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, err
	}
	return lrange.Val(), nil
}

const turnLockTTL = 600 * time.Second

// TryAcquireTurn 抢占会话的 turn 锁。false = 已有 turn 在跑（调用方据此打断）。
func (r *Redis) TryAcquireTurn(ctx context.Context, sessionID, workflowID string) (bool, error) {
	return r.client.SetNX(ctx, turnKey(sessionID), workflowID, turnLockTTL).Result()
}

// TurnHolder 返回当前持有锁的 workflowID；无锁时返回空串。
func (r *Redis) TurnHolder(ctx context.Context, sessionID string) (string, error) {
	v, err := r.client.Get(ctx, turnKey(sessionID)).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	return v, err
}

// releaseTurnScript 条件删除：只有锁仍归 expectedWorkflowID 时才删。
// 防打断竞态——旧 turn 被 Cancel 后的延迟 ReleaseTurn 不得删掉新 turn 的锁。
var releaseTurnScript = redis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("del", KEYS[1])
else
	return 0
end`)

func (r *Redis) ReleaseTurn(ctx context.Context, sessionID, expectedWorkflowID string) error {
	return releaseTurnScript.Run(ctx, r.client, []string{turnKey(sessionID)}, expectedWorkflowID).Err()
}

// PublishChanged 发"something changed"空信号（讲座 15 Watching：丢了无害）。
func (r *Redis) PublishChanged(ctx context.Context, sessionID string) error {
	return r.client.Publish(ctx, changedKey(sessionID), "").Err()
}

// SubscribeChanged 订阅变更信号；返回只读 channel 与退订函数。
// 返回前同步等订阅确认（Receive）——否则订阅尚未生效时的 Publish 会丢信号
// （本地亚毫秒时很难踩到，远端 Redis 40ms RTT 下必现）。
func (r *Redis) SubscribeChanged(ctx context.Context, sessionID string) (<-chan struct{}, func()) {
	sub := r.client.Subscribe(ctx, changedKey(sessionID))
	if _, err := sub.Receive(ctx); err != nil { // 等 SUBSCRIBE 生效
		sub.Close()
		ch := make(chan struct{})
		close(ch)
		return ch, func() {}
	}
	ch := make(chan struct{}, 16)
	go func() {
		defer close(ch)
		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-sub.Channel():
				if !ok {
					return
				}
				select {
				case ch <- struct{}{}:
				default: // 信号可合并，丢了无害
				}
			}
		}
	}()
	return ch, func() { sub.Close() }
}

// ForceSetTurn 强制占锁：打断路径用——旧 turn 已 Cancel，锁直接改写为新 workflowID。
func (r *Redis) ForceSetTurn(ctx context.Context, sessionID, workflowID string) error {
	return r.client.Set(ctx, turnKey(sessionID), workflowID, turnLockTTL).Err()
}
