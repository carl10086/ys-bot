// Package vmdriver 对 worker 屏蔽"VM 在哪、怎么唤起"。
// RemoteDriver：经 Redis catalog 找到 vmhostd，一切 VM 操作走 RPC（worker 不碰宿主）。
// 设计 docs/ys-powers/specs/2026-10-29-m2-design.md。
package vmdriver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"

	agentdv1 "ys-bot/internal/gen/agentd/v1"
	vmhostdv1 "ys-bot/internal/gen/vmhostd/v1"
	"ys-bot/internal/gen/vmhostd/v1/vmhostdv1connect"
	"ys-bot/internal/store/pg"
	"ys-bot/internal/store/redis"
)

// Driver worker 侧接口（实现可换：Remote / mock）。
type Driver interface {
	// EnsureAwake 保证用户的 VM 醒着（懒唤醒：只在首个 tool_call 前调用）。
	// 没有 sandbox 行就建行（创建时选宿主）；返回 sandboxID。
	EnsureAwake(ctx context.Context, userID string) (uuid.UUID, error)
	// Exec 在 sandbox 的 VM 里同步执行命令。
	Exec(ctx context.Context, sandboxID uuid.UUID, cmd string, timeoutSec uint32) (exitCode int32, stdout, stderr string, err error)
	// SleepVM 休眠 sandbox 的 VM（M4）：宿主侧 pause + 快照 + 停机。
	// 调用前调用方必须已完成 CAS 占坑（status=sleeping）。
	SleepVM(ctx context.Context, sandboxID uuid.UUID) error
}

// RemoteDriver 经 Redis 注册表发现 vmhostd，转发一切调用。
type RemoteDriver struct {
	PG    *pg.PG
	Redis *goredis.Client
	HTTP  *http.Client // nil → http.DefaultClient
}

func (d *RemoteDriver) httpClient() *http.Client {
	if d.HTTP != nil {
		return d.HTTP
	}
	return http.DefaultClient
}

// vmhostClient 按 hostID 从注册表查 vmhostd 地址（key 形状归 store/redis 管）。
func (d *RemoteDriver) vmhostClient(ctx context.Context, hostID string) (vmhostdv1connect.VmhostdClient, error) {
	addr, err := d.Redis.HGet(ctx, redis.VmhostAliveKey(hostID), redis.VmhostEndpointField).Result()
	if err != nil {
		return nil, fmt.Errorf("vmhostd 不在线（host=%s）: %w", hostID, err)
	}
	return vmhostdv1connect.NewVmhostdClient(d.httpClient(), "http://"+addr), nil
}

// pickHost 选宿主：任一在线 vmhostd（M2 单宿主；多宿主的调度策略 M3 再议）。
func (d *RemoteDriver) pickHost(ctx context.Context) (string, error) {
	keys, err := d.Redis.Keys(ctx, redis.VmhostAlivePrefix+"*").Result()
	if err != nil {
		return "", err
	}
	if len(keys) == 0 {
		return "", errors.New("没有在线的 vmhostd")
	}
	return strings.TrimPrefix(keys[0], redis.VmhostAlivePrefix), nil
}

func (d *RemoteDriver) EnsureAwake(ctx context.Context, userID string) (uuid.UUID, error) {
	sb, err := d.PG.GetSandboxByUser(ctx, userID)
	if errors.Is(err, pg.ErrNotFound) {
		hostID, herr := d.pickHost(ctx)
		if herr != nil {
			return uuid.Nil, herr
		}
		sb, err = d.PG.CreateSandbox(ctx, userID, hostID)
	}
	if err != nil {
		return uuid.Nil, err
	}
	if sb.Status == "awake" {
		return sb.ID, nil
	}
	c, err := d.vmhostClient(ctx, sb.HostID)
	if err != nil {
		return uuid.Nil, err
	}
	if _, err := c.EnsureVM(ctx, connect.NewRequest(&vmhostdv1.EnsureVMRequest{SandboxId: sb.ID.String()})); err != nil {
		return uuid.Nil, err
	}
	return sb.ID, nil
}

func (d *RemoteDriver) Exec(ctx context.Context, sandboxID uuid.UUID, cmd string, timeoutSec uint32) (int32, string, string, error) {
	sb, err := d.PG.GetSandboxByID(ctx, sandboxID)
	if err != nil {
		return 0, "", "", err
	}
	c, err := d.vmhostClient(ctx, sb.HostID)
	if err != nil {
		return 0, "", "", err
	}
	exec := func() (*connect.Response[agentdv1.ExecResponse], error) {
		req := connect.NewRequest(&agentdv1.ExecRequest{Cmd: cmd, TimeoutSec: timeoutSec})
		req.Header().Set("X-Sandbox-Id", sandboxID.String())
		return c.Exec(ctx, req)
	}
	resp, err := exec()
	// M4 自愈（spec Q2）：VM 非 awake（休眠竞态的残余窗口）→ EnsureVM（resume）→ 重试一次。
	// 最多一次：再失败说明不是休眠竞态，直接报错。
	if cerr := new(connect.Error); errors.As(err, &cerr) && cerr.Code() == connect.CodeFailedPrecondition {
		if _, err := c.EnsureVM(ctx, connect.NewRequest(&vmhostdv1.EnsureVMRequest{SandboxId: sb.ID.String()})); err != nil {
			return 0, "", "", fmt.Errorf("自愈 EnsureVM: %w", err)
		}
		resp, err = exec()
	}
	if err != nil {
		return 0, "", "", err
	}
	return resp.Msg.ExitCode, resp.Msg.Stdout, resp.Msg.Stderr, nil
}

// SleepVM 休眠：按 host_id 找 vmhostd 转发 SleepVM（与 Exec 同一条发现路径）。
func (d *RemoteDriver) SleepVM(ctx context.Context, sandboxID uuid.UUID) error {
	sb, err := d.PG.GetSandboxByID(ctx, sandboxID)
	if err != nil {
		return err
	}
	c, err := d.vmhostClient(ctx, sb.HostID)
	if err != nil {
		return err
	}
	_, err = c.SleepVM(ctx, connect.NewRequest(&vmhostdv1.SleepVMRequest{SandboxId: sandboxID.String()}))
	return err
}
