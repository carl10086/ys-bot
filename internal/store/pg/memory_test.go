package pg

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// M5 记忆存储的测试（spec: docs/ys-powers/specs/2026-11-02-m5-memory-design.md）。
// 覆盖：ops 事务应用（add/update/delete）+ 审计成对、hash 去重、note 过期、
// 注入四查询（profile/log/note/skills）、关键词检索、turn 消息捞取。

// mustApplyOps 应用一批 ops，失败直接 fatal。返回实际应用条数。
func mustApplyOps(t *testing.T, db *PG, userID string, ops []MemoryOp) int {
	t.Helper()
	n, err := db.ApplyMemoryOps(context.Background(), userID, ops, "pass:test")
	if err != nil {
		t.Fatalf("apply ops: %v", err)
	}
	return n
}

// countOps 数审计行数（raw 查询——审计表没有列表门面，调试场景不需要）。
func countOps(t *testing.T, db *PG, userID string) int {
	t.Helper()
	var n int
	if err := db.pool.QueryRow(context.Background(),
		"SELECT count(*) FROM memory_ops WHERE user_id = $1", userID).Scan(&n); err != nil {
		t.Fatalf("count ops: %v", err)
	}
	return n
}

func TestPG_MemoryOps_AddAndHashDedup(t *testing.T) {
	db := newTestPG(t)
	ctx := context.Background()

	n := mustApplyOps(t, db, "u-mem", []MemoryOp{
		{Op: "add", TargetType: "memory", Type: "profile", Text: "用户偏好简短回答", Importance: 8},
	})
	if n != 1 {
		t.Fatalf("applied = %d, want 1", n)
	}
	mems, err := db.ListProfileMemories(ctx, "u-mem")
	if err != nil || len(mems) != 1 {
		t.Fatalf("profile count = %d err=%v, want 1", len(mems), err)
	}
	if mems[0].Text != "用户偏好简短回答" || mems[0].Importance != 8 {
		t.Fatalf("unexpected memory: %+v", mems[0])
	}
	if got := countOps(t, db, "u-mem"); got != 1 {
		t.Fatalf("audit rows = %d, want 1", got)
	}

	// 同文本再 add：hash 去重，不加行也不审计（无事发生）
	n = mustApplyOps(t, db, "u-mem", []MemoryOp{
		{Op: "add", TargetType: "memory", Type: "profile", Text: "用户偏好简短回答", Importance: 3},
	})
	if n != 0 {
		t.Fatalf("dup add applied = %d, want 0", n)
	}
	mems, _ = db.ListProfileMemories(ctx, "u-mem")
	if len(mems) != 1 {
		t.Fatalf("after dup: profile count = %d, want 1", len(mems))
	}
	if got := countOps(t, db, "u-mem"); got != 1 {
		t.Fatalf("after dup: audit rows = %d, want 1（去重不审计）", got)
	}
}

func TestPG_MemoryOps_UpdateDeleteWithAudit(t *testing.T) {
	db := newTestPG(t)
	ctx := context.Background()

	mustApplyOps(t, db, "u-upd", []MemoryOp{
		{Op: "add", TargetType: "memory", Type: "profile", Text: "住在 Austin", Importance: 5},
	})
	mems, _ := db.ListProfileMemories(ctx, "u-upd")
	id := mems[0].ID

	// update：正文变了、updated_at 动了、审计带 before/after
	n := mustApplyOps(t, db, "u-upd", []MemoryOp{
		{Op: "update", TargetType: "memory", TargetID: id, Text: "住在 Seattle", Importance: 6},
	})
	if n != 1 {
		t.Fatalf("update applied = %d, want 1", n)
	}
	m, err := db.GetMemoryByID(ctx, id)
	if err != nil || m.Text != "住在 Seattle" {
		t.Fatalf("after update: %+v err=%v", m, err)
	}
	var before, after string
	if err := db.pool.QueryRow(ctx,
		"SELECT before_text, after_text FROM memory_ops WHERE op='update' AND target_id=$1", id).
		Scan(&before, &after); err != nil {
		t.Fatalf("audit update row: %v", err)
	}
	if before != "住在 Austin" || after != "住在 Seattle" {
		t.Fatalf("audit before/after = %q/%q", before, after)
	}

	// delete：行消失 + 审计留 before
	n = mustApplyOps(t, db, "u-upd", []MemoryOp{
		{Op: "delete", TargetType: "memory", TargetID: id},
	})
	if n != 1 {
		t.Fatalf("delete applied = %d, want 1", n)
	}
	if _, err := db.GetMemoryByID(ctx, id); err != ErrNotFound {
		t.Fatalf("after delete: err=%v, want ErrNotFound", err)
	}
	if got := countOps(t, db, "u-upd"); got != 3 {
		t.Fatalf("audit rows = %d, want 3（add+update+delete）", got)
	}

	// update/delete 对着不存在的 id：跳过不报错（LLM 可能对着已删编号发指令）
	n = mustApplyOps(t, db, "u-upd", []MemoryOp{
		{Op: "update", TargetType: "memory", TargetID: uuid.New(), Text: "x"},
		{Op: "delete", TargetType: "memory", TargetID: uuid.New()},
	})
	if n != 0 {
		t.Fatalf("missing-target applied = %d, want 0", n)
	}
}

func TestPG_SkillOps_UpsertDelete(t *testing.T) {
	db := newTestPG(t)
	ctx := context.Background()

	// add skill
	mustApplyOps(t, db, "u-sk", []MemoryOp{
		{Op: "add", TargetType: "skill", Name: "kill-152-process", Description: "在 152 上杀进程的正确姿势", Content: "先 pgrep -x ...", Importance: 5},
	})
	cat, err := db.ListSkillCatalog(ctx, "u-sk")
	if err != nil || len(cat) != 1 {
		t.Fatalf("catalog = %+v err=%v, want 1", cat, err)
	}
	if cat[0].Name != "kill-152-process" || cat[0].Description == "" {
		t.Fatalf("catalog entry: %+v", cat[0])
	}

	// 同名再写 = upsert（content 更新，审计 before 带旧 content）
	mustApplyOps(t, db, "u-sk", []MemoryOp{
		{Op: "update", TargetType: "skill", Name: "kill-152-process", Description: "杀进程 v2", Content: "pgrep -x 之后再 kill", Importance: 5},
	})
	sk, err := db.GetSkillByName(ctx, "u-sk", "kill-152-process")
	if err != nil || sk.Content != "pgrep -x 之后再 kill" || sk.Description != "杀进程 v2" {
		t.Fatalf("after upsert: %+v err=%v", sk, err)
	}
	cat, _ = db.ListSkillCatalog(ctx, "u-sk")
	if len(cat) != 1 {
		t.Fatalf("upsert 后 catalog = %d, want 1（不产生第二行）", len(cat))
	}

	// delete skill
	mustApplyOps(t, db, "u-sk", []MemoryOp{
		{Op: "delete", TargetType: "skill", Name: "kill-152-process"},
	})
	if _, err := db.GetSkillByName(ctx, "u-sk", "kill-152-process"); err != ErrNotFound {
		t.Fatalf("after delete skill: err=%v, want ErrNotFound", err)
	}
}

func TestPG_InjectionQueries(t *testing.T) {
	db := newTestPG(t)
	ctx := context.Background()

	// profile：101 条，importance = i%10 → 只回 100 条且按 importance DESC
	var ops []MemoryOp
	for i := 0; i < 101; i++ {
		ops = append(ops, MemoryOp{Op: "add", TargetType: "memory", Type: "profile",
			Text: fmt.Sprintf("事实 %03d", i), Importance: int32(i % 10)})
	}
	mustApplyOps(t, db, "u-inj", ops)
	prof, err := db.ListProfileMemories(ctx, "u-inj")
	if err != nil || len(prof) != 100 {
		t.Fatalf("profile len = %d err=%v, want 100（LIMIT 截断）", len(prof), err)
	}
	if prof[0].Importance != 9 {
		t.Fatalf("profile[0].importance = %d, want 9（DESC 排序）", prof[0].Importance)
	}

	// log：31 条 → 只回最近 30
	ops = ops[:0]
	for i := 0; i < 31; i++ {
		ops = append(ops, MemoryOp{Op: "add", TargetType: "memory", Type: "log",
			Text: fmt.Sprintf("事件 %02d", i), Importance: 5})
	}
	mustApplyOps(t, db, "u-inj", ops)
	logs, err := db.ListRecentLogMemories(ctx, "u-inj")
	if err != nil || len(logs) != 30 {
		t.Fatalf("log len = %d err=%v, want 30", len(logs), err)
	}
	if logs[0].Text != "事件 30" {
		t.Fatalf("log[0] = %q, want 最新一条", logs[0].Text)
	}

	// note：过期的不进注入；NULL expires 与未过期的进
	past := int32(-10)
	future := int32(60)
	mustApplyOps(t, db, "u-inj", []MemoryOp{
		{Op: "add", TargetType: "memory", Type: "note", Text: "过期便签", Importance: 5, TTLMinutes: &past},
		{Op: "add", TargetType: "memory", Type: "note", Text: "活跃便签", Importance: 5, TTLMinutes: &future},
		{Op: "add", TargetType: "memory", Type: "note", Text: "无期限便签", Importance: 5},
	})
	notes, err := db.ListActiveNotes(ctx, "u-inj")
	if err != nil || len(notes) != 2 {
		t.Fatalf("active notes = %d err=%v, want 2（过期被过滤）", len(notes), err)
	}
	for _, nt := range notes {
		if nt.Text == "过期便签" {
			t.Fatal("过期 note 不应出现在注入查询里")
		}
	}
	// 提取上下文（ListAllMemoriesForUser）同样不含过期 note
	all, err := db.ListAllMemoriesForUser(ctx, "u-inj")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range all {
		if m.Text == "过期便签" {
			t.Fatal("过期 note 不应进提取上下文")
		}
	}
}

func TestPG_SearchMemories(t *testing.T) {
	db := newTestPG(t)
	ctx := context.Background()

	mustApplyOps(t, db, "u-search", []MemoryOp{
		{Op: "add", TargetType: "memory", Type: "log", Text: "10-30 给 M4 做了真机验收", Importance: 5},
		{Op: "add", TargetType: "memory", Type: "profile", Text: "用户是 Go 后端", Importance: 7},
		{Op: "add", TargetType: "skill", Name: "m4-runbook", Description: "M4 验收手册", Content: "先在 152 上准备快照……", Importance: 5},
	})

	hits, err := db.SearchMemories(ctx, "u-search", "验收", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits = %d, want 2（log 一条 + skill 一条）", len(hits))
	}
	kinds := map[string]bool{}
	for _, h := range hits {
		kinds[h.Kind] = true
	}
	if !kinds["memory"] || !kinds["skill"] {
		t.Fatalf("hits 应同时覆盖 memory 和 skill: %+v", hits)
	}

	hits, _ = db.SearchMemories(ctx, "u-search", "不存在的关键词", 10)
	if len(hits) != 0 {
		t.Fatalf("miss hits = %d, want 0", len(hits))
	}
	// 别人的记忆搜不到
	hits, _ = db.SearchMemories(ctx, "u-other", "验收", 10)
	if len(hits) != 0 {
		t.Fatalf("cross-user hits = %d, want 0", len(hits))
	}
}

func TestPG_ListTurnMessages(t *testing.T) {
	db := newTestPG(t)
	ctx := context.Background()

	sess, err := db.CreateSession(ctx, "u-turn", "t")
	if err != nil {
		t.Fatal(err)
	}
	// 用户消息（无 turn_id）+ assistant 消息（有 turn_id）
	mid := uuid.New()
	userSeq, _, err := db.InsertMessage(ctx, Message{SessionID: sess.ID, Kind: KindUser, Content: []byte(`{"text":"记住我喜欢简短"}`), MsgID: &mid})
	if err != nil {
		t.Fatal(err)
	}
	asSeq, _, err := db.InsertMessage(ctx, Message{SessionID: sess.ID, Kind: KindAssistant, Content: []byte(`{"text":"好的"}`), TurnID: "turn:1"})
	if err != nil {
		t.Fatal(err)
	}
	// 另一个 turn 的 assistant 消息不应被捞到
	if _, _, err := db.InsertMessage(ctx, Message{SessionID: sess.ID, Kind: KindAssistant, Content: []byte(`{"text":"旧 turn"}`), TurnID: "turn:0"}); err != nil {
		t.Fatal(err)
	}

	msgs, err := db.ListTurnMessages(ctx, sess.ID, "turn:1", []int64{userSeq})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("turn messages = %d, want 2（用户消息靠 batch seq 兜）", len(msgs))
	}
	if msgs[0].Seq != userSeq || msgs[1].Seq != asSeq {
		t.Fatalf("顺序应按 seq 升序: %d, %d", msgs[0].Seq, msgs[1].Seq)
	}
}

// I1：并发 pass 写入同一 user——advisory lock 保证写入阶段串行，不产生矛盾终态。
func TestPG_ApplyMemoryOps_ConcurrentSameUser(t *testing.T) {
	db := newTestPG(t)
	ctx := context.Background()

	// 预置一条记忆
	if _, err := db.ApplyMemoryOps(ctx, "u-conc", []MemoryOp{
		{Op: "add", TargetType: "memory", Type: "profile", Text: "住在 Austin", Importance: 5},
	}, "seed"); err != nil {
		t.Fatal(err)
	}
	mems, _ := db.ListProfileMemories(ctx, "u-conc")
	targetID := mems[0].ID

	// 两个并发事务：一个 update，一个 delete 同一行
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := db.ApplyMemoryOps(ctx, "u-conc", []MemoryOp{
			{Op: "update", TargetType: "memory", TargetID: targetID, Text: "住在 Seattle", Importance: 8},
		}, "pass:A")
		if err != nil {
			t.Errorf("pass A: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		_, err := db.ApplyMemoryOps(ctx, "u-conc", []MemoryOp{
			{Op: "delete", TargetType: "memory", TargetID: targetID},
		}, "pass:B")
		if err != nil {
			t.Errorf("pass B: %v", err)
		}
	}()
	wg.Wait()

	// 终态：要么被 delete（行消失），要么被 update（行在且文本新）——不能出现"已删行被 update 复活"
	mems, _ = db.ListProfileMemories(ctx, "u-conc")
	if len(mems) > 1 {
		t.Fatalf("不应有多行: %d", len(mems))
	}
	if len(mems) == 1 && mems[0].Text != "住在 Seattle" {
		t.Fatalf("若行还在，文本应是更新后的: %q", mems[0].Text)
	}
	// 审计：两个 pass 的操作都应有记录（串行执行，各记各的）
	if got := countOps(t, db, "u-conc"); got < 2 {
		t.Fatalf("audit rows = %d, want >=2", got)
	}
}
