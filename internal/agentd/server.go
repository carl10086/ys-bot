// Package agentd VM 内守护进程的实现：真正执行命令的地方（e2b envd 的砍版）。
// 协议见 proto/agentd/v1/agentd.proto；设计见 docs/ys-powers/specs/2026-10-29-m2-design.md §2。
package agentd

import (
	"context"
	"errors"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"connectrpc.com/connect"

	agentdv1 "ys-sandbox/internal/gen/agentd/v1"
	"ys-sandbox/internal/gen/agentd/v1/agentdv1connect"
)

// defaultExecTimeout ExecRequest.timeout_sec=0 时的默认值。
const defaultExecTimeout = 60 * time.Second

// NewMux 返回挂了 Agentd 服务的 http.ServeMux（cmd/agentd 与测试共用）。
func NewMux() (*http.ServeMux, error) {
	mux := http.NewServeMux()
	path, handler := agentdv1connect.NewAgentdHandler(&server{})
	mux.Handle(path, handler)
	return mux, nil
}

type server struct{}

func (s *server) Health(context.Context, *connect.Request[agentdv1.HealthRequest]) (*connect.Response[agentdv1.HealthResponse], error) {
	return connect.NewResponse(&agentdv1.HealthResponse{}), nil
}

func (s *server) Exec(ctx context.Context, req *connect.Request[agentdv1.ExecRequest]) (*connect.Response[agentdv1.ExecResponse], error) {
	timeout := time.Duration(req.Msg.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = defaultExecTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", req.Msg.Cmd)
	if req.Msg.Workdir != "" {
		cmd.Dir = req.Msg.Workdir
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, connect.NewError(connect.CodeDeadlineExceeded, errors.New("exec timeout"))
	}
	resp := &agentdv1.ExecResponse{Stdout: stdout.String(), Stderr: stderr.String()}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		resp.ExitCode = int32(exitErr.ExitCode())
	default:
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(resp), nil
}
