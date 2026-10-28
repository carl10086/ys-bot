package api

import (
	"context"
	"net/http/httptest"
	"os"
	"testing"

	"ys-sandbox/internal/store/pg"
	"ys-sandbox/internal/store/redis"
)

// 真 PG/Redis（compose）+ 假 TurnStarter。
// PG 每用例 drop 重建；Redis 用固定 db=5（与 store 包测试的 db 号段隔开）。
func newTestServer(t *testing.T) (*httptest.Server, *fakeStarter, *redis.Redis, func()) {
	t.Helper()
	ctx := context.Background()

	dsn := os.Getenv("YS_TEST_PG")
	if dsn == "" {
		dsn = "postgres://ys:ys@localhost:5432/ys_sandbox"
	}
	p, err := pg.New(ctx, dsn)
	if err != nil {
		t.Skipf("PG 不可用：%v", err)
	}
	if err := p.ResetSchema(ctx); err != nil {
		t.Fatalf("reset schema: %v", err)
	}

	addr := os.Getenv("YS_TEST_REDIS")
	if addr == "" {
		addr = "localhost:6379"
	}
	r, err := redis.NewRedis(addr, 5)
	if err != nil {
		t.Skipf("Redis 不可用：%v", err)
	}

	starter := &fakeStarter{}
	srv := httptest.NewServer(New(p, r, starter, "local").Handler())
	return srv, starter, r, func() {
		srv.Close()
		p.Close()
		r.Close()
	}
}
