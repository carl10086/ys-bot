package vmdriver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"
	"github.com/redis/go-redis/v9"

	agentdv1 "ys-sandbox/internal/gen/agentd/v1"
	vmhostdv1 "ys-sandbox/internal/gen/vmhostd/v1"
	"ys-sandbox/internal/gen/vmhostd/v1/vmhostdv1connect"
	"ys-sandbox/internal/store/pg"
	storeredis "ys-sandbox/internal/store/redis"
)

// M4 Exec 自愈（spec Q2）：VM 非 awake（休眠竞态撞上）→ EnsureVM（resume）→ 重试一次。
// 测试用 connect 生成的 handler 起假 vmhostd（协议/编解码全真）+ 真 PG/redis
// （第五个测试库 ys_sandbox_test_vd，go test 包间并行，共库 ResetSchema 必撞）。

type fakeVmhostd struct {
	execCalls, ensureCalls atomic.Int32
}

func (f *fakeVmhostd) EnsureVM(context.Context, *connect.Request[vmhostdv1.EnsureVMRequest]) (*connect.Response[vmhostdv1.EnsureVMResponse], error) {
	f.ensureCalls.Add(1)
	return connect.NewResponse(&vmhostdv1.EnsureVMResponse{Endpoint: "10.200.0.2:4998"}), nil
}

func (f *fakeVmhostd) SleepVM(context.Context, *connect.Request[vmhostdv1.SleepVMRequest]) (*connect.Response[vmhostdv1.SleepVMResponse], error) {
	return connect.NewResponse(&vmhostdv1.SleepVMResponse{}), nil
}

func (f *fakeVmhostd) DestroyVM(context.Context, *connect.Request[vmhostdv1.DestroyVMRequest]) (*connect.Response[vmhostdv1.DestroyVMResponse], error) {
	return connect.NewResponse(&vmhostdv1.DestroyVMResponse{}), nil
}

func (f *fakeVmhostd) StatusVM(context.Context, *connect.Request[vmhostdv1.StatusVMRequest]) (*connect.Response[vmhostdv1.StatusVMResponse], error) {
	return connect.NewResponse(&vmhostdv1.StatusVMResponse{Status: "awake"}), nil
}

// Exec 第一次报 FailedPrecondition（模拟休眠竞态），之后成功。
func (f *fakeVmhostd) Exec(_ context.Context, req *connect.Request[agentdv1.ExecRequest]) (*connect.Response[agentdv1.ExecResponse], error) {
	if f.execCalls.Add(1) == 1 {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("VM 非 awake: sleeping"))
	}
	return connect.NewResponse(&agentdv1.ExecResponse{ExitCode: 0, Stdout: "healed"}), nil
}

func TestRemoteDriver_Exec_HealsFailedPrecondition(t *testing.T) {
	dsn := os.Getenv("YS_TEST_PG_VD")
	if dsn == "" {
		t.Skip("YS_TEST_PG_VD 未设置")
	}
	ctx := context.Background()
	db, err := pg.New(ctx, dsn)
	if err != nil {
		t.Skipf("PG 不可用: %v", err)
	}
	defer db.Close()
	if err := db.ResetSchema(ctx); err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: os.Getenv("YS_TEST_REDIS"), Password: os.Getenv("REDIS_PASSWORD")})
	defer rdb.Close()

	fake := &fakeVmhostd{}
	mux := http.NewServeMux()
	mux.Handle(vmhostdv1connect.NewVmhostdHandler(fake))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	sb, err := db.CreateSandbox(ctx, "u-heal", "host-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := rdb.HSet(ctx, storeredis.VmhostAliveKey("host-1"), storeredis.VmhostEndpointField, srv.URL[7:]).Err(); err != nil {
		t.Fatal(err)
	}

	d := &RemoteDriver{PG: db, Redis: rdb}
	_, stdout, _, err := d.Exec(ctx, sb.ID, "echo hi", 60)
	if err != nil {
		t.Fatalf("自愈后应成功: %v", err)
	}
	if stdout != "healed" {
		t.Fatalf("stdout = %q, want healed", stdout)
	}
	if fake.execCalls.Load() != 2 || fake.ensureCalls.Load() != 1 {
		t.Fatalf("exec=%d ensure=%d, want exec=2 ensure=1", fake.execCalls.Load(), fake.ensureCalls.Load())
	}
}
