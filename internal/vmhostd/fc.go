// Package vmhostd 的 fc.go：firecracker API 的最小客户端（unix socket HTTP）。
// M2：开机四步 + InstanceStart；M4：快照三调用（pause / snapshot create / snapshot load）。
// 职责划分参照 e2b orchestrator/pkg/sandbox/fc，只保留用到的调用。
package vmhostd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
)

type fcClient struct {
	hc *http.Client
}

func newFCClient(sockPath string) *fcClient {
	return &fcClient{hc: &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
			},
		},
	}}
}

// do 发一个 JSON 请求；firecracker 成功一律 204，失败返回非 2xx + 错误体。
func (c *fcClient) do(ctx context.Context, method, path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("fc %s %s: %s: %s", method, path, resp.Status, msg)
	}
	return nil
}

func (c *fcClient) put(ctx context.Context, path string, body any) error {
	return c.do(ctx, http.MethodPut, path, body)
}

func (c *fcClient) patch(ctx context.Context, path string, body any) error {
	return c.do(ctx, http.MethodPatch, path, body)
}

// bootVM 开机四步 + 启动（顺序即 firecracker 要求）。
func (c *fcClient) bootVM(ctx context.Context, kernel, bootArgs, rootfs, ifaceID, mac, tap string) error {
	if err := c.put(ctx, "/boot-source", map[string]any{
		"kernel_image_path": kernel, "boot_args": bootArgs,
	}); err != nil {
		return err
	}
	if err := c.put(ctx, "/drives/rootfs", map[string]any{
		"drive_id": "rootfs", "path_on_host": rootfs, "is_root_device": true, "is_read_only": false,
	}); err != nil {
		return err
	}
	if err := c.put(ctx, "/network-interfaces/"+ifaceID, map[string]any{
		"iface_id": ifaceID, "guest_mac": mac, "host_dev_name": tap,
	}); err != nil {
		return err
	}
	if err := c.put(ctx, "/machine-config", map[string]any{
		"vcpu_count": 1, "mem_size_mib": 512,
	}); err != nil {
		return err
	}
	return c.put(ctx, "/actions", map[string]any{"action_type": "InstanceStart"})
}

// ===== M4 快照（spec: 2026-10-30-m4-snapshot-sleep-design.md）=====

// pauseVM 暂停 vCPU——快照前必须 pause，否则快照内容不一致。
func (c *fcClient) pauseVM(ctx context.Context) error {
	return c.patch(ctx, "/vm", map[string]any{"state": "Paused"})
}

// createSnapshot 全量快照：内存 → memFile，vCPU/设备状态 → stateFile。
// 只留最新一份（调用方保证同名覆盖）。
func (c *fcClient) createSnapshot(ctx context.Context, memFile, stateFile string) error {
	return c.put(ctx, "/snapshot/create", map[string]any{
		"mem_file_path": memFile, "snapshot_path": stateFile, "snapshot_type": "Full",
	})
}

// loadSnapshot 从快照恢复并继续执行（resume）。
// 前提：tap 已按快照里的原名建好；rootfs 用休眠时的同一文件（调用方保证）。
func (c *fcClient) loadSnapshot(ctx context.Context, memFile, stateFile string) error {
	if err := c.put(ctx, "/snapshot/load", map[string]any{
		"mem_file_path": memFile, "snapshot_path": stateFile,
	}); err != nil {
		return err
	}
	return c.patch(ctx, "/vm", map[string]any{"state": "Resumed"})
}
