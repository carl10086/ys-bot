package agentd_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"

	"ys-sandbox/internal/agentd"
	agentdv1 "ys-sandbox/internal/gen/agentd/v1"
	"ys-sandbox/internal/gen/agentd/v1/agentdv1connect"
)

// 经真 RPC（httptest + connect client）验证 Exec/Health 行为。
func newClient(t *testing.T) agentdv1connect.AgentdClient {
	t.Helper()
	mux, err := agentd.NewMux()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return agentdv1connect.NewAgentdClient(srv.Client(), srv.URL)
}

func TestHealth(t *testing.T) {
	c := newClient(t)
	_, err := c.Health(context.Background(), connect.NewRequest(&agentdv1.HealthRequest{}))
	if err != nil {
		t.Fatal(err)
	}
}

func TestExec_Echo(t *testing.T) {
	c := newClient(t)
	resp, err := c.Exec(context.Background(), connect.NewRequest(&agentdv1.ExecRequest{Cmd: "echo hi"}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.ExitCode != 0 || resp.Msg.Stdout != "hi\n" {
		t.Fatalf("got exit=%d stdout=%q", resp.Msg.ExitCode, resp.Msg.Stdout)
	}
}

func TestExec_ExitCode(t *testing.T) {
	c := newClient(t)
	resp, err := c.Exec(context.Background(), connect.NewRequest(&agentdv1.ExecRequest{Cmd: "exit 3"}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.ExitCode != 3 {
		t.Fatalf("got exit=%d", resp.Msg.ExitCode)
	}
}

func TestExec_Stderr(t *testing.T) {
	c := newClient(t)
	resp, err := c.Exec(context.Background(), connect.NewRequest(&agentdv1.ExecRequest{Cmd: "echo oops >&2"}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.Stderr != "oops\n" {
		t.Fatalf("got stderr=%q", resp.Msg.Stderr)
	}
}

func TestExec_Timeout(t *testing.T) {
	c := newClient(t)
	start := time.Now()
	_, err := c.Exec(context.Background(), connect.NewRequest(&agentdv1.ExecRequest{
		Cmd: "sleep 5", TimeoutSec: 1,
	}))
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("timeout not enforced promptly")
	}
}

func TestExec_Workdir(t *testing.T) {
	c := newClient(t)
	resp, err := c.Exec(context.Background(), connect.NewRequest(&agentdv1.ExecRequest{
		Cmd: "pwd", Workdir: "/tmp",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.Stdout != "/tmp\n" {
		t.Fatalf("got stdout=%q", resp.Msg.Stdout)
	}
}
