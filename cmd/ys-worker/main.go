// ys-worker：Temporal worker，注册 TurnWorkflow 与 activities。
// M1：drain 队列 → 调 LLM → 每步写 messages → PUBLISH changed。
// 对应 spec：docs/ys-powers/specs/2026-10-28-m1-spine-design.md §5
package main

import "log"

func main() {
	log.SetPrefix("[ys-worker] ")
	log.Print("TODO T5: workflow 注册未实现")
}
