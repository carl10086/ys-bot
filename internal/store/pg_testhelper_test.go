package store

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
)

// 集成测试连 compose 起的 PG。YS_TEST_PG 未设置时用本地默认。
// 每次跑测试前重置 schema（drop + create），保证用例互不污染。
func newTestPG(t *testing.T) *PG {
	t.Helper()
	dsn := os.Getenv("YS_TEST_PG")
	if dsn == "" {
		dsn = "postgres://ys:ys@localhost:5432/ys_sandbox"
	}
	db, err := NewPG(context.Background(), dsn)
	if err != nil {
		t.Skipf("PG 不可用（先 cd deploy && docker compose up -d）：%v", err)
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
