package pg

import (
	"context"
	"testing"
)

// M1 验收（对应 data-model.md）：
// - msg_id 幂等：同一 msg_id 插入两次，第二次 inserted=false 且 seq 相同
// - seq 会话内单调递增（含无 msg_id 的 assistant 消息）
// - ListMessages(after_seq) 只返回其后的消息

func TestPG_InsertMessage_DedupByMsgID(t *testing.T) {
	db := newTestPG(t)
	ctx := context.Background()
	s := mustCreateSession(t, db, "local")

	msgID := mustParseUUID(t, "018f3c2a-0000-7000-8000-000000000001")
	m := Message{SessionID: s.ID, Kind: KindUser, Content: []byte(`{"text":"hi"}`), MsgID: &msgID}

	seq1, inserted1, err := db.InsertMessage(ctx, m)
	if err != nil || !inserted1 {
		t.Fatalf("first insert: inserted=%v err=%v", inserted1, err)
	}
	seq2, inserted2, err := db.InsertMessage(ctx, m)
	if err != nil {
		t.Fatalf("second insert err: %v", err)
	}
	if inserted2 {
		t.Fatal("duplicate msg_id must not insert")
	}
	if seq1 != seq2 {
		t.Fatalf("dedup must return original seq: got %d want %d", seq2, seq1)
	}

	msgs, err := db.ListMessages(ctx, s.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(msgs))
	}
}

func TestPG_SeqIncrementsWithinSession(t *testing.T) {
	db := newTestPG(t)
	ctx := context.Background()
	s := mustCreateSession(t, db, "local")
	other := mustCreateSession(t, db, "local")

	for i := int64(1); i <= 3; i++ {
		seq, _, err := db.InsertMessage(ctx, Message{SessionID: s.ID, Kind: KindAssistant, Content: []byte(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		if seq != i {
			t.Fatalf("insert %d: seq=%d", i, seq)
		}
	}
	// 另一个会话独立计数
	seq, _, err := db.InsertMessage(ctx, Message{SessionID: other.ID, Kind: KindUser, Content: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if seq != 1 {
		t.Fatalf("other session seq=%d, want 1", seq)
	}
}

func TestPG_ListMessages_AfterSeq(t *testing.T) {
	db := newTestPG(t)
	ctx := context.Background()
	s := mustCreateSession(t, db, "local")
	for i := 0; i < 5; i++ {
		if _, _, err := db.InsertMessage(ctx, Message{SessionID: s.ID, Kind: KindAssistant, Content: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	msgs, err := db.ListMessages(ctx, s.ID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].Seq != 4 || msgs[1].Seq != 5 {
		t.Fatalf("after_seq=3 want seq [4 5], got %+v", msgs)
	}
}

func TestPG_CreateSession_Defaults(t *testing.T) {
	db := newTestPG(t)
	s := mustCreateSession(t, db, "local")
	if s.UserID != "local" {
		t.Fatalf("user_id=%q", s.UserID)
	}
	if s.ID.Version() != 7 {
		t.Fatalf("session id must be uuidv7, got version %d", s.ID.Version())
	}
}

// 竞态兜底路径（pg.go InsertMessage 的 ON CONFLICT 分支）：
// N 个 goroutine 同 msg_id 并发插入，预检全部 miss，靠唯一约束只插一行。
// 期望：恰好一个 inserted=true；全部拿到同一个 seq；messages 只有一行。
// 败者烧掉的 seq 会在会话里留空号——无害（after_seq 是 > 比较），不断言 last_seq。
func TestPG_InsertMessage_ConcurrentSameMsgID(t *testing.T) {
	db := newTestPG(t)
	ctx := context.Background()
	s := mustCreateSession(t, db, "local")

	msgID := mustParseUUID(t, "018f3c2a-0000-7000-8000-000000000099")
	const n = 8
	type result struct {
		seq      int64
		inserted bool
		err      error
	}
	results := make(chan result, n)
	for i := 0; i < n; i++ {
		go func() {
			seq, inserted, err := db.InsertMessage(ctx, Message{
				SessionID: s.ID, Kind: KindUser, Content: []byte(`{"text":"race"}`), MsgID: &msgID,
			})
			results <- result{seq, inserted, err}
		}()
	}

	var seqs []int64
	insertedCount := 0
	for i := 0; i < n; i++ {
		r := <-results
		if r.err != nil {
			t.Fatalf("concurrent insert err: %v", r.err)
		}
		if r.inserted {
			insertedCount++
		}
		seqs = append(seqs, r.seq)
	}
	if insertedCount != 1 {
		t.Fatalf("want exactly 1 inserted, got %d (seqs=%v)", insertedCount, seqs)
	}
	for _, seq := range seqs[1:] {
		if seq != seqs[0] {
			t.Fatalf("all callers must get the winner's seq: %v", seqs)
		}
	}
	msgs, err := db.ListMessages(ctx, s.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(msgs))
	}
}
