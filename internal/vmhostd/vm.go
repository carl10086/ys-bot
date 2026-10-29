// VM 生命周期：建（cp rootfs + tap + firecracker 进程 + agentd 就绪）、毁、状态。
// VM 布局：{VMDir}/<sandbox_id>/{fc.sock, rootfs.ext4, console.log, pid}
package vmhostd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	agentdv1 "ys-sandbox/internal/gen/agentd/v1"
	"ys-sandbox/internal/gen/agentd/v1/agentdv1connect"
)

// agentdReadyTimeout 起 VM 后等 agentd health 的最长时间。
const agentdReadyTimeout = 30 * time.Second

// ensureVM 幂等：VM 醒着直接返回 endpoint，否则建 VM 并等 agentd 就绪。
// 失败时清理现场（杀进程/拆 tap/删目录）并把 sandbox 标 failed。
func (s *server) ensureVM(ctx context.Context, sandboxID uuid.UUID) (string, error) {
	sb, err := s.pg.GetSandboxByID(ctx, sandboxID)
	if err != nil {
		return "", err
	}
	dir := s.vmDir(sandboxID)
	if sb.Status == "awake" && sb.VMIP != "" && processAlive(dir) {
		return sb.VMIP, nil // 幂等命中
	}

	used, err := s.pg.ListUsedSlotsByHost(ctx, s.cfg.HostID)
	if err != nil {
		return "", err
	}
	slot := minFreeSlot(used, maxSlots)
	if slot < 0 {
		return "", errors.New("slot 池已满")
	}
	a := slotAddrs(slot)
	endpoint := a.guestIP + ":4998"

	if err := s.bootVM(ctx, sandboxID, a, dir); err != nil {
		s.cleanupVM(dir, a.tap)
		_ = s.pg.SetSandboxStatus(context.Background(), sandboxID, "failed")
		return "", err
	}
	if err := s.waitAgentd(ctx, endpoint); err != nil {
		s.cleanupVM(dir, a.tap)
		_ = s.pg.SetSandboxStatus(context.Background(), sandboxID, "failed")
		return "", fmt.Errorf("agentd 未就绪: %w", err)
	}
	// slot + awake 同事务落库；UNIQUE(host_id,slot) 冲突 = 并发抢号，由调用方重试
	if err := s.pg.SetSandboxAwake(ctx, sandboxID, slot, endpoint); err != nil {
		s.cleanupVM(dir, a.tap)
		return "", fmt.Errorf("slot 落库（可能并发抢号，可重试）: %w", err)
	}
	s.catalogSet(ctx, sandboxID, endpoint)
	return endpoint, nil
}

// bootVM 建目录、拷 rootfs、配 tap、拉 firecracker 进程并完成 API 开机。
func (s *server) bootVM(ctx context.Context, sandboxID uuid.UUID, a addrs, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	rootfs := filepath.Join(dir, "rootfs.ext4")
	if out, err := exec.Command("cp", "--reflink=auto", s.cfg.RootfsTemplate, rootfs).CombinedOutput(); err != nil {
		return fmt.Errorf("cp rootfs: %w (%s)", err, out)
	}
	if err := createTap(a); err != nil {
		return err
	}

	sock := filepath.Join(dir, "fc.sock")
	logFile, err := os.Create(filepath.Join(dir, "console.log"))
	if err != nil {
		return err
	}
	cmd := exec.Command(s.cfg.FcBin, "--api-sock", sock)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("spawn firecracker: %w", err)
	}
	_ = os.WriteFile(filepath.Join(dir, "pid"), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644)
	go func() { _ = cmd.Wait(); logFile.Close() }()

	bootArgs := "console=ttyS0 reboot=k panic=1 pci=off ys_ip=" + a.guestCIDR + " ys_gw=" + a.gateway
	fc := newFCClient(sock)
	deadline := time.Now().Add(5 * time.Second)
	for { // 等 api sock 出现
		err := fc.put(ctx, "/machine-config", map[string]any{"vcpu_count": 1, "mem_size_mib": 512})
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("firecracker api 未就绪: %w", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := fc.bootVM(ctx, s.cfg.Vmlinux, bootArgs, rootfs, "eth0", a.mac, a.tap); err != nil {
		return err
	}
	log.Printf("[vmhostd] boot sb=%s ip=%s", shortID(sandboxID), a.guestIP)
	return nil
}

// waitAgentd 轮询 agentd health 直到就绪或超时。
func (s *server) waitAgentd(ctx context.Context, endpoint string) error {
	c := agentdv1connect.NewAgentdClient(s.httpClient, "http://"+endpoint)
	deadline := time.Now().Add(agentdReadyTimeout)
	for {
		_, err := c.Health(ctx, connect.NewRequest(&agentdv1.HealthRequest{}))
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// destroyVM 杀进程、拆 tap、删目录、释放 slot（PG）、清 catalog。幂等。
func (s *server) destroyVM(ctx context.Context, sandboxID uuid.UUID) error {
	sb, err := s.pg.GetSandboxByID(ctx, sandboxID)
	if errors.Is(err, errNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	tap := ""
	if sb.Slot != nil {
		tap = slotAddrs(*sb.Slot).tap
	}
	s.cleanupVM(s.vmDir(sandboxID), tap)
	if err := s.pg.ReleaseSandbox(ctx, sandboxID); err != nil {
		return err
	}
	s.catalogDel(ctx, sandboxID)
	return nil
}

// cleanupVM 杀 firecracker 进程（pid 文件）、拆 tap、删目录。尽力而为。
func (s *server) cleanupVM(dir, tap string) {
	if b, err := os.ReadFile(filepath.Join(dir, "pid")); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			if p, err := os.FindProcess(pid); err == nil {
				_ = p.Signal(syscall.SIGKILL)
			}
		}
	}
	if tap != "" {
		_ = deleteTap(tap)
	}
	_ = os.RemoveAll(dir)
}

// reconcile 启动对账：PG 认为本宿主活着的 VM，本地没进程的标 stopped 并清场。
func (s *server) reconcile(ctx context.Context) error {
	list, err := s.pg.ListActiveSandboxesByHost(ctx, s.cfg.HostID)
	if err != nil {
		return err
	}
	for _, sb := range list {
		dir := s.vmDir(sb.ID)
		if processAlive(dir) {
			continue
		}
		tap := ""
		if sb.Slot != nil {
			tap = slotAddrs(*sb.Slot).tap
		}
		s.cleanupVM(dir, tap)
		if err := s.pg.ReleaseSandbox(ctx, sb.ID); err != nil {
			log.Printf("[vmhostd] reconcile sb=%s: %v", shortID(sb.ID), err)
			continue
		}
		s.catalogDel(ctx, sb.ID)
		log.Printf("[vmhostd] reconcile sb=%s → stopped", shortID(sb.ID))
	}
	return nil
}

// processAlive pid 文件存在且进程活着。
func processAlive(dir string) bool {
	b, err := os.ReadFile(filepath.Join(dir, "pid"))
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
