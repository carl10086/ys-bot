// vmhostd 跑在 VM 宿主上：管 firecracker 进程 + 代理 agentd（e2b orchestrator 砍版）。
// 哑基础设施，零 LLM/workflow 逻辑。
package main

import (
	"context"
	"log"
	"os"

	"ys-sandbox/internal/vmhostd"
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	cfg := vmhostd.Config{
		HostID:         envOr("HOST_ID", vmhostd.Hostname()),
		AdvertiseAddr:  envOr("ADVERTISE_ADDR", ""),
		PGDSN:          envOr("PG_DSN", "postgres://ys:ys@localhost:5432/ys_sandbox"),
		RedisAddr:      envOr("REDIS_ADDR", "localhost:6379"),
		RedisPassword:  envOr("REDIS_PASSWORD", ""),
		ListenAddr:     envOr("LISTEN_ADDR", ":50051"),
		VMDir:          envOr("VM_DIR", "/data/firecracker/vms"),
		FcBin:          envOr("FC_BIN", "/data/firecracker/firecracker"),
		Vmlinux:        envOr("VMLINUX", "/data/firecracker/vmlinux-pvm"),
		RootfsTemplate: envOr("ROOTFS_TEMPLATE", "/data/firecracker/rootfs-agentd.ext4"),
	}
	if cfg.AdvertiseAddr == "" {
		log.Fatal("ADVERTISE_ADDR 必填（写入 catalog 的本机地址，如 152.136.212.59:50051）")
	}
	log.Fatal(vmhostd.ListenAndServe(context.Background(), cfg))
}
