// ys-worker：Temporal worker，注册 TurnWorkflow 与 activities。
// M1：drain 队列 → 调 LLM → 每步写 messages → PUBLISH changed。
// 对应 spec：docs/ys-powers/specs/2026-10-28-m1-spine-design.md §5
package main

import (
	"context"
	"log"
	"os"
	"strconv"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"ys-sandbox/internal/agent"
	"ys-sandbox/internal/store/pg"
	"ys-sandbox/internal/store/redis"
	"ys-sandbox/internal/vmdriver"
	"ys-sandbox/internal/workflow"
)

func main() {
	log.SetPrefix("[ys-worker] ")
	ctx := context.Background()

	pgStore, err := pg.New(ctx, envOr("DATABASE_URL", "postgres://ys:ys@localhost:5432/ys_sandbox"))
	if err != nil {
		log.Fatalf("pg: %v", err)
	}
	rdb, err := redis.NewRedis(envOr("REDIS_ADDR", "localhost:6379"), envOr("REDIS_PASSWORD", ""), 0)
	if err != nil {
		log.Fatalf("redis: %v", err)
	}
	ag, err := agent.NewFantasy(agent.Config{
		Provider: envOr("LLM_PROVIDER", "openai"),
		BaseURL:  envOr("LLM_BASE_URL", "https://api.openai.com/v1"),
		APIKey:   os.Getenv("LLM_API_KEY"),
		Model:    envOr("LLM_MODEL", "gpt-4o-mini"),
		System:   envOr("LLM_SYSTEM", "You are a helpful assistant."),
	})
	if err != nil {
		log.Fatalf("agent: %v", err)
	}

	c, err := client.Dial(client.Options{HostPort: envOr("TEMPORAL_ADDR", "localhost:7233")})
	if err != nil {
		log.Fatalf("temporal: %v", err)
	}
	defer c.Close()

	// DEBUG_DEADLOCK_TIMEOUT_SEC：GoLand 断点调试 workflow 代码时用——把 deadlock
	// detector 超时从默认 1s 放大（如 3600），否则断点冻结协程会触发 TMPRL1101。
	// 只在 debug 配置里设；它同时会掩盖真死锁，生产勿设。
	wOpts := worker.Options{}
	if v := os.Getenv("DEBUG_DEADLOCK_TIMEOUT_SEC"); v != "" {
		if sec, err := strconv.Atoi(v); err == nil {
			wOpts.DeadlockDetectionTimeout = time.Duration(sec) * time.Second
			log.Printf("deadlock detector 超时放宽到 %ds（仅调试用）", sec)
		} else {
			log.Printf("DEBUG_DEADLOCK_TIMEOUT_SEC=%q 不是数字，忽略", v)
		}
	}
	w := worker.New(c, workflow.TaskQueue, wOpts)
	w.RegisterWorkflow(workflow.TurnWorkflow)
	// VM 工具环境（M2）：VM_DRIVER=remote 时启用；缺省无 Driver = M1 纯对话行为。
	var drv vmdriver.Driver
	if os.Getenv("VM_DRIVER") == "remote" {
		drv = &vmdriver.RemoteDriver{PG: pgStore, Redis: rdb.Client()} // 复用同一连接
		log.Print("VM_DRIVER=remote，shell 工具已启用")
	}
	w.RegisterActivity(&workflow.Activities{PG: pgStore, Redis: rdb, Agent: ag, Driver: drv})

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
