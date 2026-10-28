// ys-worker：Temporal worker，注册 TurnWorkflow 与 activities。
// M1：drain 队列 → 调 LLM → 每步写 messages → PUBLISH changed。
// 对应 spec：docs/ys-powers/specs/2026-10-28-m1-spine-design.md §5
package main

import (
	"context"
	"log"
	"os"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"ys-sandbox/internal/agent"
	"ys-sandbox/internal/store/pg"
	"ys-sandbox/internal/store/redis"
	"ys-sandbox/internal/workflow"
)

func main() {
	log.SetPrefix("[ys-worker] ")
	ctx := context.Background()

	pgStore, err := pg.New(ctx, envOr("DATABASE_URL", "postgres://ys:ys@localhost:5432/ys_sandbox"))
	if err != nil {
		log.Fatalf("pg: %v", err)
	}
	rdb, err := redis.NewRedis(envOr("REDIS_ADDR", "localhost:6379"), 0)
	if err != nil {
		log.Fatalf("redis: %v", err)
	}
	ag, err := agent.NewFantasy(agent.Config{
		BaseURL: envOr("LLM_BASE_URL", "https://api.openai.com/v1"),
		APIKey:  os.Getenv("LLM_API_KEY"),
		Model:   envOr("LLM_MODEL", "gpt-4o-mini"),
		System:  envOr("LLM_SYSTEM", "You are a helpful assistant."),
	})
	if err != nil {
		log.Fatalf("agent: %v", err)
	}

	c, err := client.Dial(client.Options{HostPort: envOr("TEMPORAL_ADDR", "localhost:7233")})
	if err != nil {
		log.Fatalf("temporal: %v", err)
	}
	defer c.Close()

	w := worker.New(c, workflow.TaskQueue, worker.Options{})
	w.RegisterWorkflow(workflow.TurnWorkflow)
	w.RegisterActivity(&workflow.Activities{PG: pgStore, Redis: rdb, Agent: ag})

	log.Printf("listening on task queue %q", workflow.TaskQueue)
	if err := w.Run(worker.InterruptCh()); err != nil {
		log.Fatalf("worker run: %v", err)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
