package redis

import (
	"context"
	"os"
	"testing"
)

// 集成测试连真实 Redis（YS_TEST_REDIS 指向，如 152 的 16379；密码取 REDIS_PASSWORD）。
// 每个用例独立 db 号防串扰。
var redisDBSeq = 0

func newTestRedis(t *testing.T) *Redis {
	t.Helper()
	addr := os.Getenv("YS_TEST_REDIS")
	if addr == "" {
		addr = "localhost:6379"
	}
	redisDBSeq++
	r, err := NewRedis(addr, os.Getenv("REDIS_PASSWORD"), redisDBSeq)
	if err != nil {
		t.Skipf("Redis 不可用（YS_TEST_REDIS/REDIS_PASSWORD 指向可用实例）：%v", err)
	}
	if err := r.client.FlushDB(context.Background()).Err(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}
