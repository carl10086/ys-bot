package pg

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// M4 休眠查询的测试（spec: docs/ys-powers/specs/2026-10-30-m4-snapshot-sleep-design.md）。
// 用 db.pool 直写 last_activity_at 构造时间场景——应用层只提供"刷成 now()"，不提供任意值。

// mustAwakeSandbox 建行并置 awake；idleAgo 为 last_activity_at 距现在的时长（0 = 不设置，保持 NULL）。
// slot 自增——UNIQUE(host_id, slot) 不许同宿主重用。
var testSlot atomic.Int32

func mustAwakeSandbox(t *testing.T, db *PG, userID string, idleAgo time.Duration) Sandbox {
	t.Helper()
	sb, err := db.CreateSandbox(context.Background(), userID, "host-1")
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if err := db.SetSandboxAwake(context.Background(), sb.ID, testSlot.Add(1), "10.200.0.2:4998"); err != nil {
		t.Fatalf("set awake: %v", err)
	}
	if idleAgo > 0 {
		_, err := db.pool.Exec(context.Background(),
			"UPDATE sandboxes SET last_activity_at = now() - $1 * interval '1 second' WHERE id = $2",
			int64(idleAgo.Seconds()), sb.ID)
		if err != nil {
			t.Fatalf("backdate last_activity_at: %v", err)
		}
	}
	sb.Status = SandboxStatusAwake
	return sb
}

func TestPG_TryStartSleep_CAS(t *testing.T) {
	db := newTestPG(t)
	ctx := context.Background()

	// 空闲 1 小时的 awake sandbox：CAS 应成功，且只能成功一次
	sb := mustAwakeSandbox(t, db, "u-cas", time.Hour)
	ok, err := db.TryStartSleep(ctx, sb.ID, 10)
	if err != nil || !ok {
		t.Fatalf("first CAS should win: ok=%v err=%v", ok, err)
	}
	got, _ := db.GetSandboxByID(ctx, sb.ID)
	if got.Status != SandboxStatusSleeping {
		t.Fatalf("status after CAS = %q, want sleeping", got.Status)
	}
	ok, err = db.TryStartSleep(ctx, sb.ID, 10)
	if err != nil || ok {
		t.Fatalf("second CAS must lose (already sleeping): ok=%v err=%v", ok, err)
	}

	// 刚有活动的 awake sandbox：CAS 抢不到
	fresh := mustAwakeSandbox(t, db, "u-fresh", 30*time.Second)
	ok, err = db.TryStartSleep(ctx, fresh.ID, 10)
	if err != nil || ok {
		t.Fatalf("fresh sandbox must not sleep: ok=%v err=%v", ok, err)
	}

	// 从未活动（NULL）的 sandbox：不休眠
	never := mustAwakeSandbox(t, db, "u-never", 0)
	ok, err = db.TryStartSleep(ctx, never.ID, 10)
	if err != nil || ok {
		t.Fatalf("NULL last_activity_at must not sleep: ok=%v err=%v", ok, err)
	}

	// stopped 的 sandbox：即使过期也不可 CAS
	stopped := mustAwakeSandbox(t, db, "u-stopped", time.Hour)
	if err := db.ReleaseSandbox(ctx, stopped.ID); err != nil {
		t.Fatal(err)
	}
	ok, err = db.TryStartSleep(ctx, stopped.ID, 10)
	if err != nil || ok {
		t.Fatalf("stopped sandbox must not sleep: ok=%v err=%v", ok, err)
	}
}

func TestPG_RefreshSandboxActivity(t *testing.T) {
	db := newTestPG(t)
	ctx := context.Background()

	sb := mustAwakeSandbox(t, db, "u-refresh", time.Hour)
	if err := db.RefreshSandboxActivity(ctx, sb.ID); err != nil {
		t.Fatal(err)
	}
	var ts time.Time
	if err := db.pool.QueryRow(ctx, "SELECT last_activity_at FROM sandboxes WHERE id = $1", sb.ID).Scan(&ts); err != nil {
		t.Fatal(err)
	}
	if time.Since(ts) > time.Minute {
		t.Fatalf("last_activity_at not refreshed: %v ago", time.Since(ts))
	}
	// 刷新后 CAS 立即抢不到
	ok, err := db.TryStartSleep(ctx, sb.ID, 10)
	if err != nil || ok {
		t.Fatalf("CAS right after refresh must fail: ok=%v err=%v", ok, err)
	}
}

func TestPG_ListSleepCandidates(t *testing.T) {
	db := newTestPG(t)
	ctx := context.Background()

	expired := mustAwakeSandbox(t, db, "u-expired", time.Hour)   // 应命中
	mustAwakeSandbox(t, db, "u-active", 30*time.Second)          // awake 但未过期
	sleeping := mustAwakeSandbox(t, db, "u-sleeping", time.Hour) // 过期但已 sleeping
	if _, err := db.TryStartSleep(ctx, sleeping.ID, 10); err != nil {
		t.Fatal(err)
	}
	otherHost := mustAwakeSandbox(t, db, "u-other", time.Hour) // 过期 awake，跨宿主也命中（sweeper 是全局的）
	if _, err := db.pool.Exec(ctx, "UPDATE sandboxes SET host_id = 'host-2' WHERE id = $1", otherHost.ID); err != nil {
		t.Fatal(err)
	}

	list, err := db.ListSleepCandidates(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	got := map[uuid.UUID]string{}
	for _, sb := range list {
		got[sb.ID] = sb.HostID
	}
	if len(got) != 2 || got[expired.ID] != "host-1" || got[otherHost.ID] != "host-2" {
		t.Fatalf("candidates = %v, want exactly expired+otherHost with host ids", got)
	}
}

func TestPG_SnapshotRef_SetAndClearedOnRelease(t *testing.T) {
	db := newTestPG(t)
	ctx := context.Background()

	sb := mustAwakeSandbox(t, db, "u-ref", time.Hour)
	wantRef := "snap.vmstate"
	if err := db.SetSandboxSnapshotRef(ctx, sb.ID, &wantRef); err != nil {
		t.Fatal(err)
	}
	var ref *string
	if err := db.pool.QueryRow(ctx, "SELECT snapshot_ref FROM sandboxes WHERE id = $1", sb.ID).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	if ref == nil || *ref != "snap.vmstate" {
		t.Fatalf("snapshot_ref = %v, want snap.vmstate", ref)
	}

	// ReleaseSandbox（销毁/对账清场）必须连快照引用一起清
	if err := db.ReleaseSandbox(ctx, sb.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.pool.QueryRow(ctx, "SELECT snapshot_ref FROM sandboxes WHERE id = $1", sb.ID).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	if ref != nil {
		t.Fatalf("snapshot_ref after release = %v, want NULL", *ref)
	}
}
