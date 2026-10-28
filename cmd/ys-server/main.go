// ys-server：HTTP API + 内嵌 SPA，对外入口。
// M1：POST /sessions、POST /sessions/{id}/messages（幂等/入队/打断）、GET /sessions/{id}/messages?after_seq=
// 对应 spec：docs/ys-powers/specs/2026-10-28-m1-spine-design.md
package main

import (
	"log"
	"os"
)

func main() {
	log.SetPrefix("[ys-server] ")
	log.Printf("TODO T6: HTTP API 未实现（addr=%s）", envOr("ADDR", ":8080"))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
