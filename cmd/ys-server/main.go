// ys-server：HTTP API + 内嵌 SPA，对外入口。
// M1：POST /api/sessions、POST /api/sessions/{id}/messages（幂等/入队/打断）、
// GET /api/sessions/{id}/messages?after_seq=、GET /api/sessions/{id}/events（SSE 空信号）。
// 对应 spec：docs/ys-powers/specs/2026-10-28-m1-spine-design.md
package main

import (
	"context"
	"log"
	"log/slog"
	"net/http"
	"os"

	"go.temporal.io/sdk/client"

	"ys-sandbox/internal/api"
	"ys-sandbox/internal/store/pg"
	"ys-sandbox/internal/store/redis"
	"ys-sandbox/internal/workflow"
	"ys-sandbox/web"
)

func main() {
	ctx := context.Background()
	logger := slog.Default()

	pgStore, err := pg.New(ctx, envOr("DATABASE_URL", "postgres://ys:ys@localhost:5432/ys_sandbox"))
	if err != nil {
		log.Fatalf("pg: %v", err)
	}
	if err := pgStore.ApplySchema(ctx); err != nil { // 幂等建表，不引入 migrate 工具（选型 #9）
		log.Fatalf("apply schema: %v", err)
	}
	rdb, err := redis.NewRedis(envOr("REDIS_ADDR", "localhost:6379"), 0)
	if err != nil {
		log.Fatalf("redis: %v", err)
	}
	tc, err := client.Dial(client.Options{HostPort: envOr("TEMPORAL_ADDR", "localhost:7233")})
	if err != nil {
		log.Fatalf("temporal: %v", err)
	}
	defer tc.Close()

	handler := api.New(pgStore, rdb, workflow.TemporalStarter{Client: tc}, envOr("YS_USER_ID", "local")).
		WithStatic(web.Dist()).
		Handler()

	addr := envOr("ADDR", ":8080")
	logger.Info("ys-server listening", "addr", addr)
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatalf("http: %v", err)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
