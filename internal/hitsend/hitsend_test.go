package hitsend

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"ys-sandbox/internal/store/pg"
	"ys-sandbox/internal/store/redis"
)

// M4 续期测试（spec: 2026-10-30-m4-snapshot-sleep-design.md Q1）。
// hitsend 用独立测试库 ys_sandbox_test_hs（go test 包间并行，共库 ResetSchema 必撞）。
// 时间断言用 raw pool 直查——last_activity_at 不外漏到 Sandbox 结构（gen 列最小化）。

type fakeStarter struct{ started []string }

func (f *fakeStarter) StartTurn(_ context.Context, workflowID, _ string) error {
	f.started = append(f.started, workflowID)
	return nil
}

func (f *fakeStarter) CancelTurn(context.Context, string) error { return nil }

func newTestService(t *testing.T) (*Service, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("YS_TEST_PG_HS")
	if dsn == "" {
		t.Skip("YS_TEST_PG_HS 未设置（hitsend 专用测试库）")
	}
	db, err := pg.New(context.Background(), dsn)
	if err != nil {
		t.Skipf("PG 不可用: %v", err)
	}
	if err := db.ResetSchema(context.Background()); err != nil {
		t.Fatalf("reset: %v", err)
	}
	rdb, err := redis.NewRedis(os.Getenv("YS_TEST_REDIS"), os.Getenv("REDIS_PASSWORD"), 0)
	if err != nil {
		t.Skipf("redis 不可用: %v", err)
	}
	raw, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(); rdb.Close(); raw.Close() })
	return &Service{PG: db, Redis: rdb, Starter: &fakeStarter{}}, raw
}

func lastActivityAt(t *testing.T, raw *pgxpool.Pool, userID string) *time.Time {
	t.Helper()
	var ts *time.Time
	if err := raw.QueryRow(context.Background(),
		"SELECT last_activity_at FROM sandboxes WHERE user_id = $1", userID).Scan(&ts); err != nil {
		t.Fatal(err)
	}
	return ts
}

func TestSubmit_RefreshesSandboxActivity(t *testing.T) {
	svc, raw := newTestService(t)
	ctx := context.Background()

	sess, err := svc.PG.CreateSession(ctx, "u-m4", "")
	if err != nil {
		t.Fatal(err)
	}
	sb, err := svc.PG.CreateSandbox(ctx, "u-m4", "host-1")
	if err != nil {
		t.Fatal(err)
	}
	if ts := lastActivityAt(t, raw, "u-m4"); ts != nil {
		t.Fatalf("新建 sandbox 的 last_activity_at 应为 NULL，got %v", ts)
	}

	msgID := uuid.New()
	if _, _, err := svc.Submit(ctx, sess.ID, msgID, "看看 /etc"); err != nil {
		t.Fatal(err)
	}
	ts := lastActivityAt(t, raw, "u-m4")
	if ts == nil || time.Since(*ts) > time.Minute {
		t.Fatalf("Submit 后 last_activity_at 应被刷成 now，got %v", ts)
	}
	_ = sb
}
