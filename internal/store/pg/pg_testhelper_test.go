package pg

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
)

// 集成测试连真实 PG（YS_TEST_PG 指向）。注意 ResetSchema 会 drop 重建，
// 必须指向测试专用库（152 上的 ys_sandbox_test），严禁指向业务库。
// 每次跑测试前重置 schema（drop + create），保证用例互不污染。
func newTestPG(t *testing.T) *PG {
	t.Helper()
	dsn := os.Getenv("YS_TEST_PG")
	if dsn == "" {
		dsn = "postgres://ys:ys@localhost:5432/ys_sandbox_test"
	}
	db, err := New(context.Background(), dsn)
	if err != nil {
		t.Skipf("PG 不可用（YS_TEST_PG 指向测试专用库）：%v", err)
	}
	if err := db.ResetSchema(context.Background()); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func mustCreateSession(t *testing.T, db *PG, userID string) Session {
	t.Helper()
	s, err := db.CreateSession(context.Background(), userID, "")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return s
}

func mustParseUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	u, err := uuid.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
