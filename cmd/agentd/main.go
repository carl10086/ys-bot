// agentd 跑在 microVM 里，是工具真正执行的地方（e2b envd 的砍版）。
// 静态编译（CGO_ENABLED=0）后烤进 rootfs，由 systemd 拉起，监听 :4998。
package main

import (
	"log"
	"net/http"

	"ys-bot/internal/agentd"
)

func main() {
	mux, err := agentd.NewMux()
	if err != nil {
		log.Fatal(err)
	}
	log.Print("[agentd] listening on :4998")
	log.Fatal(http.ListenAndServe(":4998", mux))
}
