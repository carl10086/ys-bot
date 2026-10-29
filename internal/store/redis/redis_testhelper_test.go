package redis

import (
	"context"
	"os"
	"testing"
)

// 集成测试连 compose 起的 Redis，每个用例独立 db 号防串扰。
var redisDBSeq = 0

func newTestRedis(t *testing.T) *Redis {
	t.Helper()
	addr := os.Getenv("YS_TEST_REDIS")
	if addr == "" {
		addr = "localhost:6379"
	}
	redisDBSeq++
	r, err := NewRedis(addr, "", redisDBSeq)
	if err != nil {
		t.Skipf("Redis 不可用（先 cd deploy && docker compose up -d）：%v", err)
	}
	if err := r.client.FlushDB(context.Background()).Err(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}
