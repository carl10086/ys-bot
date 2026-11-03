// VM 生命周期：建（cp rootfs + tap + firecracker 进程 + agentd 就绪）、毁、状态、
// 休眠/恢复（M4：pause + 快照落盘 / 从快照原地恢复）。
// VM 布局：{VMDir}/<sandbox_id>/{fc.sock, rootfs.ext4, console.log, pid, snap.mem, snap.vmstate}
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

	agentdv1 "ys-bot/internal/gen/agentd/v1"
	"ys-bot/internal/gen/agentd/v1/agentdv1connect"
	"ys-bot/internal/store/pg"
)

// agentdReadyTimeout 起 VM 后等 agentd health 的最长时间。
const agentdReadyTimeout = 30 * time.Second

// ensureVM 幂等：VM 醒着直接返回 endpoint；有快照走 resume（M4），否则全新 boot。
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

	// M4：有快照 → resume（原地恢复）；失败降级全新 boot——快照只是优化，不是正确性依赖
	if hasSnapshot(dir) {
		endpoint, err := s.resumeVM(ctx, sb, dir)
		if err == nil {
			return endpoint, nil
		}
		log.Printf("[vmhostd] resume sb=%s 失败，降级全新 boot: %v", shortID(sb.ID), err)
		tap := ""
		if sb.Slot != nil {
			tap = slotAddrs(*sb.Slot).tap
		}
		s.cleanupVM(dir, tap) // 坏快照随目录一起删，防下次再踩
		_ = s.pg.SetSandboxSnapshotRef(ctx, sb.ID, nil)
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
	if err := s.spawnFC(dir); err != nil {
		return err
	}

	bootArgs := "console=ttyS0 reboot=k panic=1 pci=off ys_ip=" + a.guestCIDR + " ys_gw=" + a.gateway
	fc := newFCClient(filepath.Join(dir, "fc.sock"))
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

// spawnFC 起 firecracker 进程（API socket 模式），写 pid 文件、console 输出进 console.log。
// resume 场景目录里有上次的 fc.sock 残留——先删，否则 firecracker 绑定失败直接退出。
func (s *server) spawnFC(dir string) error {
	sock := filepath.Join(dir, "fc.sock")
	_ = os.Remove(sock)
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
	return nil
}

// ===== M4 快照休眠/恢复（spec: docs/ys-powers/specs/2026-10-30-m4-snapshot-sleep-design.md）=====

// 快照文件名（只留最新一份，同名覆盖）。
const (
	snapMemFile   = "snap.mem"
	snapStateFile = "snap.vmstate"
)

// hasSnapshot snap.vmstate 存在 = 有快照可恢复（resume 分支的存在性判据）。
func hasSnapshot(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, snapStateFile))
	return err == nil
}

// sleepVM 休眠：pause → 全量快照落盘 → 杀 fc → 拆 tap。
// VM 目录/rootfs/快照保留，slot 不释放（guest 网络配置冻在内存快照里，resume 必须同名同 IP）。
// status 的 CAS（awake→sleeping）由调用方（SleepWorkflow）负责，本函数只管宿主侧动作 + 快照引用。
func (s *server) sleepVM(ctx context.Context, sandboxID uuid.UUID) error {
	sb, err := s.pg.GetSandboxByID(ctx, sandboxID)
	if err != nil {
		return err
	}
	dir := s.vmDir(sandboxID)
	fc := newFCClient(filepath.Join(dir, "fc.sock"))
	if err := fc.pauseVM(ctx); err != nil {
		return fmt.Errorf("pause: %w", err)
	}
	if err := fc.createSnapshot(ctx, filepath.Join(dir, snapMemFile), filepath.Join(dir, snapStateFile)); err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	killFC(dir)
	if sb.Slot != nil {
		_ = deleteTap(slotAddrs(*sb.Slot).tap)
	}
	ref := snapStateFile
	if err := s.pg.SetSandboxSnapshotRef(ctx, sandboxID, &ref); err != nil {
		return fmt.Errorf("snapshot_ref 落库: %w", err)
	}
	return nil
}

// resumeVM 从快照原地恢复：杀残留进程（休眠中途崩溃的防御）→ 建同名 tap → 起 fc → load → resumed。
// 网络身份（tap/IP/MAC）必须复用 sb.Slot——guest 的内核参数与网络栈冻在内存快照里。
func (s *server) resumeVM(ctx context.Context, sb pg.Sandbox, dir string) (string, error) {
	if sb.Slot == nil {
		return "", errors.New("sleeping 但 slot 为空（不该发生，M4 约定休眠保 slot）")
	}
	a := slotAddrs(*sb.Slot)
	endpoint := a.guestIP + ":4998"
	killFC(dir)
	if err := createTap(a); err != nil {
		return "", fmt.Errorf("recreate tap: %w", err)
	}
	if err := s.spawnFC(dir); err != nil {
		return "", err
	}
	fc := newFCClient(filepath.Join(dir, "fc.sock"))
	deadline := time.Now().Add(5 * time.Second)
	for { // 等 api sock 出现
		err := fc.loadSnapshot(ctx, filepath.Join(dir, snapMemFile), filepath.Join(dir, snapStateFile))
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			// 附 console.log 尾部：目录马上被降级路径删掉，这是 fc 现场的唯一留证
			return "", fmt.Errorf("load snapshot: %w; console 尾部: %s", err, consoleTail(dir))
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := s.waitAgentd(ctx, endpoint); err != nil {
		return "", fmt.Errorf("agentd 未就绪: %w", err)
	}
	if err := s.pg.SetSandboxAwake(ctx, sb.ID, *sb.Slot, endpoint); err != nil {
		return "", fmt.Errorf("awake 落库: %w", err)
	}
	log.Printf("[vmhostd] resume sb=%s ip=%s（原地恢复）", shortID(sb.ID), a.guestIP)
	return endpoint, nil
}

// consoleTail 读 fc console.log 的尾部（resume 失败排障用；文件可能很大只取末尾）。
func consoleTail(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "console.log"))
	if err != nil {
		return "(读不到 console.log)"
	}
	if len(b) > 500 {
		b = b[len(b)-500:]
	}
	return string(b)
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

// destroyVM 杀进程、拆 tap、删目录、释放 slot（PG）。幂等。
func (s *server) destroyVM(ctx context.Context, sandboxID uuid.UUID) error {
	sb, err := s.pg.GetSandboxByID(ctx, sandboxID)
	if errors.Is(err, pg.ErrNotFound) {
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
	return nil
}

// killFC 杀目录里 pid 文件记的 firecracker 进程（尽力而为）。
func killFC(dir string) {
	if b, err := os.ReadFile(filepath.Join(dir, "pid")); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			if p, err := os.FindProcess(pid); err == nil {
				_ = p.Signal(syscall.SIGKILL)
			}
		}
	}
}

// cleanupVM 杀 firecracker 进程、拆 tap、删目录。尽力而为。
func (s *server) cleanupVM(dir, tap string) {
	killFC(dir)
	if tap != "" {
		_ = deleteTap(tap)
	}
	_ = os.RemoveAll(dir)
}

// reconcileAction reconcile 对单个 sandbox 的处置（纯决策，classifySandbox 产出）。
type reconcileAction int

const (
	reconcileKeep       reconcileAction = iota // 一切正常，不动
	reconcileClean                             // 账活实死：清场 + 转 stopped
	reconcileOrphanStop                        // sleeping 但快照丢：转 stopped（目录留给排查）
)

// classifySandbox reconcile 的处置决策（M4：sleeping 没进程是正常的，只看快照在不在）。
func classifySandbox(status string, processAlive, hasSnap bool) reconcileAction {
	switch status {
	case "creating", "awake":
		if !processAlive {
			return reconcileClean
		}
	case "sleeping":
		if !hasSnap {
			return reconcileOrphanStop
		}
	}
	return reconcileKeep
}

// reconcile 启动对账：PG 账本 vs 宿主现场，不一致按 classifySandbox 处置。
func (s *server) reconcile(ctx context.Context) error {
	list, err := s.pg.ListActiveSandboxesByHost(ctx, s.cfg.HostID)
	if err != nil {
		return err
	}
	for _, sb := range list {
		dir := s.vmDir(sb.ID)
		if classifySandbox(sb.Status, processAlive(dir), hasSnapshot(dir)) != reconcileClean {
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
		log.Printf("[vmhostd] reconcile sb=%s → stopped", shortID(sb.ID))
	}
	// M4：sleeping VM 没进程是正常的（快照在目录里）；快照丢了才转 stopped——下次懒唤醒走全新 boot
	sleeping, err := s.pg.ListSleepingSandboxesByHost(ctx, s.cfg.HostID)
	if err != nil {
		return err
	}
	for _, sb := range sleeping {
		if classifySandbox(sb.Status, false, hasSnapshot(s.vmDir(sb.ID))) != reconcileOrphanStop {
			continue
		}
		if err := s.pg.ReleaseSandbox(ctx, sb.ID); err != nil {
			log.Printf("[vmhostd] reconcile sb=%s: %v", shortID(sb.ID), err)
			continue
		}
		log.Printf("[vmhostd] reconcile sb=%s 快照丢失 → stopped", shortID(sb.ID))
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
