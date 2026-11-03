package workflow

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"ys-bot/internal/store/pg"
)

// M4 续期（spec Q1）：ExecTool 每次执行开头刷新 sandbox 的 last_activity_at——
// 长命令期间 turn 持续续期，是 never-mid-turn 成立的前提。

func TestExecTool_RefreshesSandboxActivity(t *testing.T) {
	dsn := os.Getenv("YS_TEST_PG_WF")
	if dsn == "" {
		t.Skip("YS_TEST_PG_WF 未设置")
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
	raw, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()

	sb, err := db.CreateSandbox(ctx, "u-m4", "host-1")
	if err != nil {
		t.Fatal(err)
	}

	a := &Activities{PG: db, Driver: fakeDriver{}}
	if _, err := a.ExecTool(ctx, ExecToolInput{
		SandboxID: sb.ID.String(), Name: "shell", Input: `{"cmd":"echo hi"}`,
	}); err != nil {
		t.Fatal(err)
	}

	var ts *time.Time
	if err := raw.QueryRow(ctx, "SELECT last_activity_at FROM sandboxes WHERE id = $1", sb.ID).Scan(&ts); err != nil {
		t.Fatal(err)
	}
	if ts == nil || time.Since(*ts) > time.Minute {
		t.Fatalf("ExecTool 后 last_activity_at 应被刷成 now，got %v", ts)
	}
}
