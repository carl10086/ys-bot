package workflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/temporal"
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
		{Op: "update", Target: 1, Text: "新偏好", Importance: 7}, // 命中 m1
		{Op: "delete", Target: 2},                             // 命中 m2
		{Op: "delete", Target: 3},                             // 命中 skill（按 name）
		{Op: "add", Type: "log", Text: "新事件", Importance: 5},  // add 不需要 target
		{Op: "none"},                          // 显式不动
		{Op: "update", Target: 99, Text: "x"}, // 越界跳过
		{Op: "add", Type: "log", Text: ""},    // 空文本跳过
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

// 分层更新不能靠 update.type 把旧 Note 变成 Log：存储行没有转层语义。
func TestResolveOps_RejectsCrossLayerTarget(t *testing.T) {
	noteID, profileID := uuid.New(), uuid.New()
	refs := numberMap([]pg.Memory{
		{ID: noteID, Type: "note", Text: "M5 验收进行中"},
		{ID: profileID, Type: "profile", Text: "偏好简短"},
	}, nil)

	ops := resolveOps([]rawMemoryOp{
		{Op: "update", Target: 1, Type: "log", Text: "M5 验收完成"},
		{Op: "delete", Target: 2, Type: "log"},
		{Op: "update", Target: 1, Type: "note", Text: "M5 验收接近完成"},
		{Op: "delete", Target: 2}, // 省略 type 时沿用目标层，兼容已有输出
	}, refs)
	if len(ops) != 2 {
		t.Fatalf("跨层操作应被拒绝，ops = %+v", ops)
	}
	if ops[0].Type != "note" || ops[0].TargetID != noteID || ops[1].Type != "profile" {
		t.Fatalf("同层解析结果错误: %+v", ops)
	}
}

func TestResolveOps_NoteRequiresPositiveTTL(t *testing.T) {
	negative := int32(-1)
	zero := int32(0)
	positive := int32(60)
	ops := resolveOps([]rawMemoryOp{
		{Op: "add", Type: "note", Text: "无期限 Note"},
		{Op: "add", Type: "note", Text: "负 TTL", TTLMinutes: &negative},
		{Op: "add", Type: "note", Text: "零 TTL", TTLMinutes: &zero},
		{Op: "add", Type: "profile", Text: "错误携带 TTL", TTLMinutes: &positive},
		{Op: "add", Type: "note", Text: "合法 Note", TTLMinutes: &positive},
	}, nil)
	if len(ops) != 1 || ops[0].Text != "合法 Note" {
		t.Fatalf("Note TTL 校验失败: %+v", ops)
	}
}

// 提取器需要明确的基准时间，否则“今天/上周”无法稳定落成绝对日期。
func TestExtractMemories_IncludesReferenceTime(t *testing.T) {
	fake := &captureMemoryAgent{text: `{"ops":[]}`}
	a := &Activities{MemoryAgent: fake}
	ref := time.Date(2026, 11, 2, 15, 4, 5, 0, time.UTC)
	_, err := a.ExtractMemories(context.Background(), ExtractInput{ReferenceTime: ref})
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.messages) != 1 || !strings.Contains(fake.messages[0].Content, "2026-11-02T15:04:05Z") {
		t.Fatalf("提取输入缺少 reference time: %+v", fake.messages)
	}
}

type captureMemoryAgent struct {
	text     string
	messages []agent.Message
}

func (f *captureMemoryAgent) Step(_ context.Context, msgs []agent.Message, _ []agent.ToolDef) (agent.StepResult, error) {
	f.messages = msgs
	return agent.StepResult{Text: f.text}, nil
}

// 每用户队列必须完整串行执行 pass，而不是只在最终写库时串行。
func TestMemoryPassQueueWorkflow_SerializesWholePass(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(MemoryPassWorkflow)
	env.RegisterActivity(&Activities{})

	var calls []string
	env.OnActivity("LoadTurnMessages", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { calls = append(calls, "load:"+args.Get(2).(string)) }).
		Return([]pg.Message{{Kind: pg.KindUser, Content: []byte(`{"text":"real task"}`)}}, nil)
	env.OnActivity("LoadMemoryVersion", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { calls = append(calls, "version") }).Return(int64(0), nil)
	env.OnActivity("LoadMemories", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { calls = append(calls, "memories") }).Return([]pg.Memory{}, nil)
	env.OnActivity("LoadSkillCatalog", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { calls = append(calls, "skills") }).Return([]pg.SkillCatalogEntry{}, nil)
	env.OnActivity("ExtractMemories", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { calls = append(calls, "extract") }).Return([]pg.MemoryOp{}, nil)

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(MemoryPassEnqueueSignal, MemoryPassInput{UserID: "u", SessionID: "s", TurnID: "turn-1"})
		env.SignalWorkflow(MemoryPassEnqueueSignal, MemoryPassInput{UserID: "u", SessionID: "s", TurnID: "turn-1"}) // 重试重复投递
		env.SignalWorkflow(MemoryPassEnqueueSignal, MemoryPassInput{UserID: "u", SessionID: "s", TurnID: "turn-2"})
	}, 0)
	env.RegisterDelayedCallback(env.CancelWorkflow, time.Minute)
	env.ExecuteWorkflow(MemoryPassQueueWorkflow, MemoryPassQueueInput{UserID: "u"})
	err := env.GetWorkflowError()
	// SDK test harness 在当前版本会把该取消结束包装成 ScheduleToClose；生产路径仍显式返回 CanceledError。
	if err == nil || (!temporal.IsCanceledError(err) && !strings.Contains(err.Error(), "deadline exceeded")) {
		t.Fatalf("队列取消未向上传播: %v", err)
	}

	want := []string{"load:turn-1", "version", "memories", "skills", "extract", "load:turn-2", "version", "memories", "skills", "extract"}
	if strings.Join(calls, ",") != strings.Join(want, ",") {
		t.Fatalf("pass 未按完整区间串行:\n got %v\nwant %v", calls, want)
	}
}

func TestMemoryPassPending_DedupAndBounded(t *testing.T) {
	pending := []MemoryPassInput{{TurnID: "old"}}
	pending, duplicate, dropped := appendMemoryPassPending(pending, MemoryPassInput{TurnID: "old"})
	if !duplicate || dropped != 0 || len(pending) != 1 {
		t.Fatalf("重复 TurnID 应忽略: pending=%v duplicate=%v dropped=%d", pending, duplicate, dropped)
	}
	for i := 0; i < memoryPassQueueBatchSize+20; i++ {
		pending, _, dropped = appendMemoryPassPending(pending, MemoryPassInput{TurnID: fmt.Sprintf("turn-%d", i)})
	}
	if len(pending) != memoryPassQueueBatchSize {
		t.Fatalf("pending 超过 CAN 上限: %d", len(pending))
	}
	if pending[0].TurnID == "old" {
		t.Fatal("溢出应丢弃最旧项")
	}
}

func TestMemoryPassWorkflow_ReextractsAfterSnapshotConflict(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{})
	env.OnActivity("LoadTurnMessages", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return([]pg.Message{{Kind: pg.KindUser, Content: []byte(`{"text":"M5"}`)}}, nil)
	env.OnActivity("LoadMemoryVersion", mock.Anything, "u").Return(int64(7), nil).Once()
	env.OnActivity("LoadMemoryVersion", mock.Anything, "u").Return(int64(8), nil).Once()
	env.OnActivity("LoadMemories", mock.Anything, "u").
		Return([]pg.Memory{{Type: "profile", Text: "旧快照"}}, nil).Once()
	env.OnActivity("LoadMemories", mock.Anything, "u").
		Return([]pg.Memory{{Type: "profile", Text: "新快照"}}, nil).Once()
	env.OnActivity("LoadSkillCatalog", mock.Anything, "u").Return([]pg.SkillCatalogEntry{}, nil)
	var extracts []ExtractInput
	env.OnActivity("ExtractMemories", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { extracts = append(extracts, args.Get(1).(ExtractInput)) }).
		Return([]pg.MemoryOp{{Op: "add", TargetType: "memory", Type: "log", Text: "2026-11-02 M5 完成"}}, nil)
	env.OnActivity("ApplyMemoryOps", mock.Anything, "u", mock.Anything, "pass:turn-conflict", int64(7)).
		Return(ApplyMemoryOpsResult{Conflict: true}, nil).Once()
	env.OnActivity("ApplyMemoryOps", mock.Anything, "u", mock.Anything, "pass:turn-conflict", int64(8)).
		Return(ApplyMemoryOpsResult{Applied: 1}, nil).Once()

	env.ExecuteWorkflow(MemoryPassWorkflow, MemoryPassInput{UserID: "u", SessionID: "s", TurnID: "turn-conflict"})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if len(extracts) != 2 || extracts[1].Memories[0].Text != "新快照" {
		t.Fatalf("冲突后必须使用新快照重新提取: %+v", extracts)
	}
	env.AssertExpectations(t)
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

// systemPrompt 接线（真 PG）：session→user→四路查询→组装；过期 note 不进。
func TestActivities_SystemPrompt(t *testing.T) {
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
	out, err := a.systemPrompt(ctx, sess.ID)
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
