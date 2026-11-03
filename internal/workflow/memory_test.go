package workflow

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/testsuite"

	"ys-bot/internal/agent"
	"ys-bot/internal/store/pg"
)

// M5 提取管线测试（spec: 2026-11-02-m5-memory-design.md Q7）。
// 纯函数（序号映射/ops 解析）+ activity 级（提取失败）+ workflow 级（真 PG 端到端）。

// fakeMemoryAgent 返回固定文本的 LLM 假身（提取模型）。
type fakeMemoryAgent struct {
	text string
	err  error
}

func (f fakeMemoryAgent) Step(context.Context, []agent.Message, []agent.ToolDef) (agent.StepResult, error) {
	if f.err != nil {
		return agent.StepResult{}, f.err
	}
	return agent.StepResult{Text: f.text}, nil
}

func newMemoryTestPG(t *testing.T) (*pg.PG, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("YS_TEST_PG_WF")
	if dsn == "" {
		t.Skip("YS_TEST_PG_WF 未设置")
	}
	db, err := pg.New(context.Background(), dsn)
	if err != nil {
		t.Skipf("PG 不可用: %v", err)
	}
	if err := db.ResetSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(); raw.Close() })
	return db, raw
}

// 序号映射：UUID 不喂给 LLM，只喂 1,2,3…（mem0 防幻觉技巧）。
func TestNumberMapAndResolve(t *testing.T) {
	m1, m2 := uuid.New(), uuid.New()
	mems := []pg.Memory{
		{ID: m1, Type: "profile", Text: "旧偏好"},
		{ID: m2, Type: "log", Text: "旧事件"},
	}
	skills := []pg.SkillCatalogEntry{{Name: "kill-152", Description: "杀进程"}}

	refs := numberMap(mems, skills)
	if len(refs) != 3 {
		t.Fatalf("refs = %d, want 3", len(refs))
	}

	ops := resolveOps([]rawMemoryOp{
		{Op: "update", Target: 1, Text: "新偏好", Importance: 7},          // 命中 m1
		{Op: "delete", Target: 2},                                        // 命中 m2
		{Op: "delete", Target: 3},                                        // 命中 skill（按 name）
		{Op: "add", Type: "log", Text: "新事件", Importance: 5},           // add 不需要 target
		{Op: "none"},                                                     // 显式不动
		{Op: "update", Target: 99, Text: "x"},                            // 越界跳过
		{Op: "add", Type: "log", Text: ""},                               // 空文本跳过
	}, refs)

	if len(ops) != 4 {
		t.Fatalf("resolved = %d, want 4（none/越界/空文本跳过）", len(ops))
	}
	if ops[0].TargetID != m1 || ops[0].Op != "update" || ops[0].TargetType != "memory" {
		t.Fatalf("ops[0] = %+v", ops[0])
	}
	if ops[1].TargetID != m2 || ops[1].Op != "delete" {
		t.Fatalf("ops[1] = %+v", ops[1])
	}
	if ops[2].TargetType != "skill" || ops[2].Name != "kill-152" {
		t.Fatalf("ops[2] = %+v（skill 按 name 寻址）", ops[2])
	}
	if ops[3].Type != "log" || ops[3].Text != "新事件" {
		t.Fatalf("ops[3] = %+v", ops[3])
	}
}

// 提取 activity：LLM 返回坏 JSON / 报错 → activity 失败（不进 workflow 重试循环，直接单测）。
func TestExtractMemories_BadLLMOutput(t *testing.T) {
	a := &Activities{MemoryAgent: fakeMemoryAgent{text: "这不是 JSON"}}
	if _, err := a.ExtractMemories(context.Background(), ExtractInput{}); err == nil {
		t.Fatal("坏 JSON 应报错（交 Temporal 重试）")
	}

	a = &Activities{MemoryAgent: fakeMemoryAgent{err: errors.New("llm down")}}
	if _, err := a.ExtractMemories(context.Background(), ExtractInput{}); err == nil {
		t.Fatal("LLM 报错应透传")
	}

	// ```json fence 包装是常态，要能剥
	a = &Activities{MemoryAgent: fakeMemoryAgent{text: "```json\n{\"ops\": []}\n```"}}
	ops, err := a.ExtractMemories(context.Background(), ExtractInput{})
	if err != nil || len(ops) != 0 {
		t.Fatalf("fence 包装应解析成功: ops=%v err=%v", ops, err)
	}
}

// workflow 端到端：真 PG + fake 提取模型。覆盖 spec 验收 2/3 的单测形态。
func TestMemoryPassWorkflow_EndToEnd(t *testing.T) {
	db, raw := newMemoryTestPG(t)
	ctx := context.Background()

	sess, err := db.CreateSession(ctx, "u-pass", "t")
	if err != nil {
		t.Fatal(err)
	}
	// 本轮消息：用户消息（无 turn_id，靠 batch seq）+ assistant 回复（有 turn_id）
	uid := uuid.New()
	userSeq, _, err := db.InsertMessage(ctx, pg.Message{SessionID: sess.ID, Kind: pg.KindUser,
		Content: []byte(`{"text":"我搬到 Seattle 了"}`), MsgID: &uid})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.InsertMessage(ctx, pg.Message{SessionID: sess.ID, Kind: pg.KindAssistant,
		Content: []byte(`{"text":"好的"}`), TurnID: "turn:test-1"}); err != nil {
		t.Fatal(err)
	}
	// 已有记忆：旧地址（待更新）。source 用 "seed"，避免审计统计混入 pass: 前缀
	if _, err := db.ApplyMemoryOps(ctx, "u-pass", []pg.MemoryOp{
		{Op: "add", TargetType: "memory", Type: "profile", Text: "住在 Austin", Importance: 5},
	}, "seed"); err != nil {
		t.Fatal(err)
	}
	mems, _ := db.ListProfileMemories(ctx, "u-pass")
	oldID := mems[0].ID
	_ = oldID

	// 提取模型返回：update 1（旧地址→新地址）+ add 一条 log + delete 越界编号（跳过）
	fake := fakeMemoryAgent{text: `{"ops": [
		{"op": "update", "target": 1, "text": "住在 Seattle", "importance": 8},
		{"op": "add", "type": "log", "text": "2026-11-02 用户搬到 Seattle", "importance": 5},
		{"op": "delete", "target": 42}
	]}`}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{PG: db, MemoryAgent: fake})

	env.ExecuteWorkflow(MemoryPassWorkflow, MemoryPassInput{
		UserID: "u-pass", SessionID: sess.ID.String(), TurnID: "turn:test-1", BatchSeqs: []int64{userSeq},
	})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}

	mems, _ = db.ListProfileMemories(ctx, "u-pass")
	if len(mems) != 1 || mems[0].Text != "住在 Seattle" {
		t.Fatalf("profile after pass = %+v（验收 3：update 不并存）", mems)
	}
	logs, _ := db.ListRecentLogMemories(ctx, "u-pass")
	if len(logs) != 1 || !strings.Contains(logs[0].Text, "Seattle") {
		t.Fatalf("log after pass = %+v", logs)
	}
	// 审计：update + add 两行，source=pass:turn:test-1；越界 delete 跳过不审计
	var n int
	var src string
	row := raw.QueryRow(ctx,
		"SELECT count(*), min(source) FROM memory_ops WHERE user_id='u-pass' AND source LIKE 'pass:%'")
	if err := row.Scan(&n, &src); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("audit rows = %d, want 2（验收 2）", n)
	}
	if src != "pass:turn:test-1" {
		t.Fatalf("audit source = %q, want pass:<turnID>", src)
	}
}

// BuildSystemPrompt 接线（真 PG）：session→user→四路查询→组装；过期 note 不进。
func TestActivities_BuildSystemPrompt(t *testing.T) {
	db, _ := newMemoryTestPG(t)
	ctx := context.Background()

	sess, err := db.CreateSession(ctx, "u-prompt", "t")
	if err != nil {
		t.Fatal(err)
	}
	past := int32(-10)
	if _, err := db.ApplyMemoryOps(ctx, "u-prompt", []pg.MemoryOp{
		{Op: "add", TargetType: "memory", Type: "profile", Text: "偏好简短回答", Importance: 9},
		{Op: "add", TargetType: "memory", Type: "log", Text: "2026-11-02 做了 M5 设计", Importance: 5},
		{Op: "add", TargetType: "memory", Type: "note", Text: "活跃便签", Importance: 5},
		{Op: "add", TargetType: "memory", Type: "note", Text: "过期便签", Importance: 5, TTLMinutes: &past},
		{Op: "add", TargetType: "skill", Name: "m5-runbook", Description: "验收手册", Content: "全文不注入", Importance: 5},
	}, "seed"); err != nil {
		t.Fatal(err)
	}

	a := &Activities{PG: db, BasePrompt: "BASE"}
	out, err := a.BuildSystemPrompt(ctx, sess.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"BASE", "偏好简短回答", "2026-11-02 做了 M5 设计", "活跃便签", "m5-runbook", "验收手册"} {
		if !strings.Contains(out, want) {
			t.Fatalf("prompt 缺 %q:\n%s", want, out)
		}
	}
	for _, notWant := range []string{"过期便签", "全文不注入"} {
		if strings.Contains(out, notWant) {
			t.Fatalf("prompt 不应含 %q", notWant)
		}
	}
}
