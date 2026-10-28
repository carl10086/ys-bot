package redis

import (
	"context"
	"testing"
	"time"
)

// M1 Redis 三结构（全是协调态，可丢可重建，data-model.md）：
// - 会话队列：LIST，turn 开始时 drain 成一批
// - turn 锁：SET NX EX，保证一会话一 turn；锁被占 = 有 turn 在跑
// - 变更信号：Pub/Sub 空信号，丢了无害

func TestRedis_QueueDrainReturnsBatch(t *testing.T) {
	r := newTestRedis(t)
	ctx := context.Background()
	q := Queue{SessionID: "s1"}

	for _, m := range []string{"m1", "m2", "m3"} {
		if err := r.PushMessage(ctx, q, m); err != nil {
			t.Fatal(err)
		}
	}
	batch, err := r.DrainQueue(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) != 3 || batch[0] != "m1" || batch[2] != "m3" {
		t.Fatalf("want [m1 m2 m3] FIFO, got %v", batch)
	}
	// drain 后队列必须为空（消息已在 PG，队列可丢）
	batch, err = r.DrainQueue(ctx, q)
	if err != nil || len(batch) != 0 {
		t.Fatalf("after drain want empty, got %v err=%v", batch, err)
	}
}

func TestRedis_TurnLockMutualExclusion(t *testing.T) {
	r := newTestRedis(t)
	ctx := context.Background()

	ok, err := r.TryAcquireTurn(ctx, "s1", "wf-1")
	if err != nil || !ok {
		t.Fatalf("first acquire: ok=%v err=%v", ok, err)
	}
	// 第二次抢占失败 = 有 turn 在跑（触发打断逻辑的依据）
	ok, err = r.TryAcquireTurn(ctx, "s1", "wf-2")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("second acquire must fail while turn running")
	}
	holder, err := r.TurnHolder(ctx, "s1")
	if err != nil || holder != "wf-1" {
		t.Fatalf("holder=%q err=%v, want wf-1", holder, err)
	}

	// 持有者不符时不得释放（防打断竞态删掉新 turn 的锁）
	if err := r.ReleaseTurn(ctx, "s1", "wf-other"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := r.TryAcquireTurn(ctx, "s1", "wf-3"); ok {
		t.Fatal("release with wrong holder must keep the lock")
	}
	if err := r.ReleaseTurn(ctx, "s1", "wf-1"); err != nil {
		t.Fatal(err)
	}
	ok, err = r.TryAcquireTurn(ctx, "s1", "wf-2")
	if err != nil || !ok {
		t.Fatalf("acquire after release: ok=%v err=%v", ok, err)
	}
}

func TestRedis_PublishChangedDeliversEmptySignal(t *testing.T) {
	r := newTestRedis(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ch, unsubscribe := r.SubscribeChanged(ctx, "s1")
	defer unsubscribe()

	if err := r.PublishChanged(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ch:
	case <-ctx.Done():
		t.Fatal("signal not received")
	}
}
