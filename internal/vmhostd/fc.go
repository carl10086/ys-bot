// firecracker API 的最小客户端（unix socket HTTP，只覆盖 M2 需要的 5 个调用）。
// 职责划分参照 e2b orchestrator/pkg/sandbox/fc，但只保留开机四步 + InstanceStart。
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

// put 发一个 PUT JSON；firecracker 成功一律 204，失败返回非 2xx + 错误体。
func (c *fcClient) put(ctx context.Context, path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://localhost"+path, bytes.NewReader(b))
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
		return fmt.Errorf("fc PUT %s: %s: %s", path, resp.Status, msg)
	}
	return nil
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
