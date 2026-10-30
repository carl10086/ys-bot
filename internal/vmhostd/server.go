// Package vmhostd VM 宿主上的哑基础设施：管 firecracker 进程生命周期 + 代理 agentd 调用。
// 零 LLM/workflow 逻辑。协议 proto/vmhostd/v1/vmhostd.proto；
// 设计 docs/ys-powers/specs/2026-10-29-m2-design.md。
package vmhostd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"

	agentdv1 "ys-sandbox/internal/gen/agentd/v1"
	"ys-sandbox/internal/gen/agentd/v1/agentdv1connect"
	vmhostdv1 "ys-sandbox/internal/gen/vmhostd/v1"
	"ys-sandbox/internal/gen/vmhostd/v1/vmhostdv1connect"
	"ys-sandbox/internal/store/pg"
	"ys-sandbox/internal/store/redis"
)

// Config vmhostd 运行配置（env 注入）。
type Config struct {
	HostID         string // 本宿主标识（写 sandboxes.host_id / 心跳 key）
	AdvertiseAddr  string // 写入 catalog 的本机地址，如 "152.136.212.59:15051"
	PGDSN          string
	RedisAddr      string
	RedisPassword  string
	ListenAddr     string // 默认 :50051
	VMDir          string // VM 目录根，如 /data/firecracker/vms
	FcBin          string // firecracker 二进制
	Vmlinux        string // guest 内核（必须 PVM 版）
	RootfsTemplate string // rootfs 模板（含 agentd）
}

// NewMux 装配服务（启动时先 reconcile，再开心跳 goroutine）。
func NewMux(ctx context.Context, cfg Config) (*http.ServeMux, error) {
	db, err := pg.New(ctx, cfg.PGDSN)
	if err != nil {
		return nil, err
	}
	rdb := goredis.NewClient(&goredis.Options{Addr: cfg.RedisAddr, Password: cfg.RedisPassword})
	s := &server{cfg: cfg, pg: db, rdb: rdb, httpClient: http.DefaultClient}
	if err := s.reconcile(ctx); err != nil {
		return nil, fmt.Errorf("reconcile: %w", err)
	}
	go s.heartbeatLoop(ctx)
	mux := http.NewServeMux()
	path, handler := vmhostdv1connect.NewVmhostdHandler(s)
	mux.Handle(path, handler)
	return mux, nil
}

type server struct {
	cfg        Config
	pg         *pg.PG
	rdb        *goredis.Client
	httpClient *http.Client
}

func (s *server) vmDir(id uuid.UUID) string {
	return filepath.Join(s.cfg.VMDir, id.String())
}

// shortID 日志用短 sandbox id（docker 短容器 ID / git 短 SHA 风格）。
func shortID(id uuid.UUID) string {
	s := id.String()
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// parseSandboxID 解析请求里的 sandbox id；非法时返回 InvalidArgument。
func parseSandboxID(raw string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return id, nil
}

func (s *server) EnsureVM(ctx context.Context, req *connect.Request[vmhostdv1.EnsureVMRequest]) (*connect.Response[vmhostdv1.EnsureVMResponse], error) {
	id, err := parseSandboxID(req.Msg.SandboxId)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	endpoint, err := s.ensureVM(ctx, id)
	if err != nil {
		log.Printf("[vmhostd] ensure sb=%s err=%v dur=%s", shortID(id), err, time.Since(start).Round(time.Millisecond))
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// dur 即故事：毫秒级 = 幂等命中（VM 复用），秒级 = 真起了 VM（另有 boot 日志）
	log.Printf("[vmhostd] ensure sb=%s endpoint=%s dur=%s", shortID(id), endpoint, time.Since(start).Round(time.Millisecond))
	return connect.NewResponse(&vmhostdv1.EnsureVMResponse{Endpoint: endpoint}), nil
}

func (s *server) DestroyVM(ctx context.Context, req *connect.Request[vmhostdv1.DestroyVMRequest]) (*connect.Response[vmhostdv1.DestroyVMResponse], error) {
	id, err := parseSandboxID(req.Msg.SandboxId)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	if err := s.destroyVM(ctx, id); err != nil {
		log.Printf("[vmhostd] destroy sb=%s err=%v dur=%s", shortID(id), err, time.Since(start).Round(time.Millisecond))
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	log.Printf("[vmhostd] destroy sb=%s dur=%s", shortID(id), time.Since(start).Round(time.Millisecond))
	return connect.NewResponse(&vmhostdv1.DestroyVMResponse{}), nil
}

func (s *server) StatusVM(ctx context.Context, req *connect.Request[vmhostdv1.StatusVMRequest]) (*connect.Response[vmhostdv1.StatusVMResponse], error) {
	id, err := parseSandboxID(req.Msg.SandboxId)
	if err != nil {
		return nil, err
	}
	sb, err := s.pg.GetSandboxByID(ctx, id)
	if errors.Is(err, pg.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	log.Printf("[vmhostd] status sb=%s status=%s", shortID(id), sb.Status)
	return connect.NewResponse(&vmhostdv1.StatusVMResponse{Status: sb.Status, Endpoint: sb.VMIP}), nil
}

// SleepVM 休眠（M4）：pause → 快照落盘 → 杀 fc → 拆 tap；目录/快照保留，slot 不释放。
// status 的 CAS 由调用方（SleepWorkflow）先做；本 RPC 只管宿主侧动作。
func (s *server) SleepVM(ctx context.Context, req *connect.Request[vmhostdv1.SleepVMRequest]) (*connect.Response[vmhostdv1.SleepVMResponse], error) {
	id, err := parseSandboxID(req.Msg.SandboxId)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	if err := s.sleepVM(ctx, id); err != nil {
		log.Printf("[vmhostd] sleep sb=%s err=%v dur=%s", shortID(id), err, time.Since(start).Round(time.Millisecond))
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	log.Printf("[vmhostd] sleep sb=%s dur=%s", shortID(id), time.Since(start).Round(time.Millisecond))
	return connect.NewResponse(&vmhostdv1.SleepVMResponse{}), nil
}

// Exec 代理：按 sandbox_id 转发到对应 VM 的 agentd（VM tap IP 只有本宿主能路由，
// 所以代理必须长在 vmhostd 上——设计文档"拓扑决策"）。
func (s *server) Exec(ctx context.Context, req *connect.Request[agentdv1.ExecRequest]) (*connect.Response[agentdv1.ExecResponse], error) {
	raw := req.Header().Get("X-Sandbox-Id")
	id, err := uuid.Parse(raw)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("header X-Sandbox-Id 缺失或非法"))
	}
	sb, err := s.pg.GetSandboxByID(ctx, id)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if sb.Status != "awake" || sb.VMIP == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("VM 非 awake: %s", sb.Status))
	}
	c := agentdv1connect.NewAgentdClient(s.httpClient, "http://"+sb.VMIP)
	start := time.Now()
	resp, err := c.Exec(ctx, req)
	if err != nil {
		log.Printf("[vmhostd] exec sb=%s cmd=%q err=%v dur=%s", shortID(id), req.Msg.Cmd, err, time.Since(start).Round(time.Millisecond))
		return nil, err
	}
	log.Printf("[vmhostd] exec sb=%s cmd=%q exit=%d dur=%s", shortID(id), req.Msg.Cmd, resp.Msg.ExitCode, time.Since(start).Round(time.Millisecond))
	return resp, nil
}

// heartbeatLoop 每 10s 续一次 TTL=30s 的存活 key；进程死了 key 自动消失（穷人版注册中心）。
// key/field 形状归 store/redis 管（读方是 vmdriver）。
func (s *server) heartbeatLoop(ctx context.Context) {
	key := redis.VmhostAliveKey(s.cfg.HostID)
	tick := func() {
		if err := s.rdb.HSet(ctx, key, redis.VmhostEndpointField, s.cfg.AdvertiseAddr, "ts", time.Now().Unix()).Err(); err == nil {
			s.rdb.Expire(ctx, key, 30*time.Second)
		}
	}
	tick()
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
			tick()
		}
	}
}

// ListenAndServe 入口辅助。
func ListenAndServe(ctx context.Context, cfg Config) error {
	mux, err := NewMux(ctx, cfg)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return err
	}
	log.Printf("[vmhostd] host=%s listening on %s (advertise %s)", cfg.HostID, cfg.ListenAddr, cfg.AdvertiseAddr)
	return http.Serve(ln, mux)
}

// Hostname 默认 HostID。
func Hostname() string {
	h, _ := os.Hostname()
	return h
}
