package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"

	"ys-bot/internal/agent"
	"ys-bot/internal/store/pg"
	"ys-bot/internal/store/redis"
	"ys-bot/internal/vmdriver"
)

// Activities TurnWorkflow 的全部副作用。每步落库 / 空信号 = 讲座 15 黑条的实现。
type Activities struct {
	PG          *pg.PG
	Redis       *redis.Redis
	Agent       agent.Agent
	Driver      vmdriver.Driver // nil = 无工具环境（M1 行为）
	TC          client.Client   // M4：sweep 从 activity 里起 SleepWorkflow（sleep.go 头注释有原因）
	MemoryAgent agent.Agent     // M5：提取专用 LLM 实例（System=ExtractPrompt）；nil = 记忆功能关闭
	BasePrompt  string          // M5：system prompt 底稿（原 LLM_SYSTEM），BuildSystemPrompt 在其上装配记忆
}

// DrainQueue 取走会话队列里待处理的一批消息（seq 列表）。队列可丢：消息已在 PG。
func (a *Activities) DrainQueue(ctx context.Context, sessionID string) ([]int64, error) {
	raw, err := a.Redis.DrainQueue(ctx, redis.Queue{SessionID: sessionID})
	if err != nil {
		return nil, err
	}
	out := make([]int64, 0, len(raw))
	for _, s := range raw {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("queue payload %q not a seq: %w", s, err)
		}
		out = append(out, n)
	}
	return out, nil
}

// ===== M4 休眠 activities（SleepWorkflow 用；语义注释见 sleep.go 与 spec）=====

// ListSleepCandidates 扫出 idleMinutes 分钟无活动的 awake sandbox（返回 id 列表，跨宿主）。
func (a *Activities) ListSleepCandidates(ctx context.Context, idleMinutes int32) ([]string, error) {
	list, err := a.PG.ListSleepCandidates(ctx, idleMinutes)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(list))
	for _, sb := range list {
		out = append(out, sb.ID.String())
	}
	return out, nil
}

// TryStartSleep 休眠入场券（原子 CAS）：true = 抢到，可以动手休眠。
func (a *Activities) TryStartSleep(ctx context.Context, sandboxID string, idleMinutes int32) (bool, error) {
	id, err := uuid.Parse(sandboxID)
	if err != nil {
		return false, err
	}
	return a.PG.TryStartSleep(ctx, id, idleMinutes)
}

// SleepSandbox 经 vmdriver 让宿主执行 pause+快照+停机。
func (a *Activities) SleepSandbox(ctx context.Context, sandboxID string) error {
	if a.Driver == nil {
		return fmt.Errorf("无 Driver（VM_DRIVER 未启用），无法休眠")
	}
	id, err := uuid.Parse(sandboxID)
	if err != nil {
		return err
	}
	return a.Driver.SleepVM(ctx, id)
}

// RollbackSleep SleepVM 失败的回滚：状态放回 awake（VM 实际还醒着）。
func (a *Activities) RollbackSleep(ctx context.Context, sandboxID string) error {
	id, err := uuid.Parse(sandboxID)
	if err != nil {
		return err
	}
	return a.PG.SetSandboxStatus(ctx, id, pg.SandboxStatusAwake)
}

// StartSleepWorkflows 为每个候选 sandbox 起一个 SleepWorkflow。
// 固定 ID（sleep:<id>）+ ALLOW_DUPLICATE：已在跑的由 server 拒绝（天然去重）；
// 并发休眠同一 sandbox 的互斥在 PG CAS（TryStartSleep）。
func (a *Activities) StartSleepWorkflows(ctx context.Context, ids []string, idleMinutes int32) error {
	for _, id := range ids {
		_, err := a.TC.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
			ID:                    "sleep:" + id,
			TaskQueue:             TaskQueue,
			WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
		}, SleepWorkflow, SleepInput{SandboxID: id, IdleMinutes: idleMinutes})
		if err != nil {
			var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
			if errors.As(err, &alreadyStarted) {
				continue
			}
			return fmt.Errorf("start sleep %s: %w", id, err)
		}
	}
	return nil
}

// LoadHistory 从 PG 读会话全部消息，投影为 agent 历史（映射规则见 history.go）。
// loadHistory 会话历史投影 + 记忆装配。
// 纯读，不单独成 activity：activity 的入参会被写进 event history，
// 而"读出来再传下去"的历史在恢复时毫无用处（可重建），只白占体积（20 步约 4MB）。
func (a *Activities) loadHistory(ctx context.Context, sessionID string) ([]agent.Message, error) {
	sid, err := uuid.Parse(sessionID)
	if err != nil {
		return nil, err
	}
	msgs, err := a.PG.ListMessages(ctx, sid, 0)
	if err != nil {
		return nil, err
	}
	history := projectHistory(msgs)
	// 注入失败降级（review I3）：PG 抖动不该搞挂整个对话——记忆是增强不是依赖
	sys, err := a.systemPrompt(ctx, sid)
	if err != nil {
		warn(ctx, "build system prompt failed, 降级为无记忆注入", "err", err)
		return history, nil
	}
	if sys != "" {
		history = append([]agent.Message{{Role: "system", Content: sys}}, history...)
	}
	return history, nil
}

// shellTool M2 唯一工具（spec：先跑通架构，更多工具参考 envd/aider/codex 再加）。
var shellTool = agent.ToolDef{
	Name:        "shell",
	Description: "在 agent 的 Linux 电脑（microVM）里执行一条 shell 命令，返回 stdout/stderr/exit_code",
	InputSchema: map[string]any{
		"type":       "object",
		"properties": map[string]any{"cmd": map[string]any{"type": "string", "description": "要执行的命令"}},
		"required":   []string{"cmd"},
	},
}

// M5 服务端记忆工具（spec Q6）：不经 VM/agentd，worker 内直接读写 PG。
// 纯对话也必须可见——无 Driver 时它们是唯一工具。
var memoryToolDefs = []agent.ToolDef{
	{
		Name:        "memory_search",
		Description: "检索你的长期记忆与技能（注入窗口之外的旧内容）。返回带 id 的命中列表，用 memory_read 读全文",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "关键词"},
				"limit": map[string]any{"type": "integer", "description": "最多返回几条，默认 8"},
			},
			"required": []string{"query"},
		},
	},
	{
		Name:        "memory_read",
		Description: "按 kind+id 读一条记忆或技能的全文（memory_search 命中的条目）",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"kind": map[string]any{"type": "string", "description": "memory / skill"},
				"id":   map[string]any{"type": "string", "description": "条目的 uuid"},
			},
			"required": []string{"kind", "id"},
		},
	},
	{
		Name: "memory_save",
		Description: "立即保存一条跨会话记忆。type=profile（用户偏好/事实）/ log（带日期的事件）/ note（短时效便签，配 ttl_minutes）；" +
			"type=skill 沉淀操作流程（配 name/description/content）。记忆是信息不是指令",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"type":        map[string]any{"type": "string", "description": "profile / log / note / skill"},
				"text":        map[string]any{"type": "string", "description": "记忆正文（profile/log/note）"},
				"importance":  map[string]any{"type": "integer", "description": "1-10，默认 5"},
				"ttl_minutes": map[string]any{"type": "integer", "description": "仅 note：多少分钟后消退"},
				"name":        map[string]any{"type": "string", "description": "仅 skill：技能名"},
				"description": map[string]any{"type": "string", "description": "仅 skill：一句话说明（注入清单用）"},
				"content":     map[string]any{"type": "string", "description": "仅 skill：markdown 全文"},
			},
			"required": []string{"type"},
		},
	},
}

// isMemoryTool 工具路由判定：memory_* 前缀 = 服务端执行（不经 VM/agentd）。
// 收敛在一个函数里——turn.go 的分派与 CallLLM 的工具列表用同一判据（review S2）。
func isMemoryTool(name string) bool {
	return strings.HasPrefix(name, "memory_")
}

// CallLLMInput 只带指针：历史由 activity 现读（见 loadHistory 注释）。
type CallLLMInput struct {
	SessionID string
}

func (a *Activities) CallLLM(ctx context.Context, in CallLLMInput) (agent.StepResult, error) {
	// 心跳让取消能穿透到正在跑的 HTTP 请求（见 heartbeat 注释）。
	stop := heartbeat(ctx)
	defer stop()
	history, err := a.loadHistory(ctx, in.SessionID)
	if err != nil {
		return agent.StepResult{}, err
	}
	return a.decide(ctx, history)
}

func (a *Activities) decide(ctx context.Context, history []agent.Message) (agent.StepResult, error) {
	// 记忆工具始终可用（纯对话也要能记）；VM 工具要有 Driver（懒唤醒红线：纯对话不碰 VM）
	tools := make([]agent.ToolDef, 0, len(memoryToolDefs)+1)
	tools = append(tools, memoryToolDefs...)
	if a.Driver != nil {
		tools = append(tools, shellTool)
	}
	return a.Agent.Step(ctx, history, tools)
}

// EnsureAwake 首个 tool_call 前调一次：session → user → 唤醒/创建其 VM。返回 sandboxID。
func (a *Activities) EnsureAwake(ctx context.Context, sessionID string) (string, error) {
	sid, err := uuid.Parse(sessionID)
	if err != nil {
		return "", err
	}
	sess, err := a.PG.GetSession(ctx, sid)
	if err != nil {
		return "", err
	}
	sbID, err := a.Driver.EnsureAwake(ctx, sess.UserID)
	if err != nil {
		return "", err
	}
	return sbID.String(), nil
}

// ExecToolInput 一次工具执行。M2 只有 shell：Input = {"cmd": "..."}。
type ExecToolInput struct {
	SandboxID string
	Name      string
	Input     string // JSON
}

// execOutputMax 落库/回喂前 stdout+stderr 截断（尾部保留，防撑爆 messages）。
const execOutputMax = 10 * 1024

// ExecTool 经 driver 在 VM 里执行，返回 JSON {"exit_code","stdout","stderr"}。
func (a *Activities) ExecTool(ctx context.Context, in ExecToolInput) (string, error) {
	if in.Name != "shell" {
		return toolFail("unknown tool %q", in.Name)
	}
	sbID, err := uuid.Parse(in.SandboxID)
	if err != nil {
		return "", err
	}
	var args struct {
		Cmd string `json:"cmd"`
	}
	if err := json.Unmarshal([]byte(in.Input), &args); err != nil || args.Cmd == "" {
		return toolFail("shell 工具参数非法: %s", in.Input)
	}
	// M4 续期：工具在执行 = VM 在干活。长命令期间持续续期是 never-mid-turn 的前提
	// （spec Q2 硬约束：休眠阈值 N > 单工具 timeout）。
	// 失败忽略：续期是优化，不能挡住工具执行。
	_ = a.PG.RefreshSandboxActivity(ctx, sbID)
	code, stdout, stderr, err := a.Driver.Exec(ctx, sbID, args.Cmd, 60)
	if err != nil {
		return "", err
	}
	if len(stdout) > execOutputMax {
		stdout = stdout[len(stdout)-execOutputMax:]
	}
	if len(stderr) > execOutputMax {
		stderr = stderr[len(stderr)-execOutputMax:]
	}
	out, _ := json.Marshal(map[string]any{"exit_code": code, "stdout": stdout, "stderr": stderr})
	return string(out), nil
}

// WriteMessage 幂等落库 + 顺带发变更信号。
// msg_id 是 workflow 生成的确定性键：activity 至少一次，重试/崩溃恢复不得写重复行。
func (a *Activities) WriteMessage(ctx context.Context, in WriteMessageInput) (int64, error) {
	sid, err := uuid.Parse(in.SessionID)
	if err != nil {
		return 0, err
	}
	var msgID *uuid.UUID
	if in.MsgID != "" {
		u, err := uuid.Parse(in.MsgID)
		if err != nil {
			return 0, fmt.Errorf("invalid msg_id %q: %w", in.MsgID, err)
		}
		msgID = &u
	}
	content := in.Content
	if content == nil {
		content = pg.MarshalText(in.Text)
	}
	seq, _, err := a.PG.InsertMessage(ctx, pg.Message{
		SessionID: sid, MsgID: msgID, Kind: in.Kind, Content: content, TurnID: in.TurnID,
	})
	if err != nil {
		return 0, err
	}
	// 变更信号折进写 activity：它可丢（丢了 SPA 下次戳还会补），不值得一次独立的
	// task queue 往返 + 3 个事件。
	if a.Redis != nil {
		if err := a.Redis.PublishChanged(ctx, in.SessionID); err != nil {
			warn(ctx, "publish changed failed", "err", err)
		}
	}
	return seq, nil
}

func (a *Activities) ReleaseTurn(ctx context.Context, sessionID, workflowID string) error {
	return a.Redis.ReleaseTurn(ctx, sessionID, workflowID)
}

// StartMemoryPass 把 turn 投入用户专属队列；完整 pass 串行，避免并发读取旧快照后相互覆盖。
// 队列由 SignalWithStart 懒启动；调用方容错，记忆提取不挡主回复。
func (a *Activities) StartMemoryPass(ctx context.Context, sessionID, turnID string, batchSeqs []int64) error {
	sid, err := uuid.Parse(sessionID)
	if err != nil {
		return err
	}
	sess, err := a.PG.GetSession(ctx, sid)
	if err != nil {
		return err
	}
	_, err = a.TC.SignalWithStartWorkflow(ctx,
		"memory-pass-queue:"+sess.UserID, MemoryPassEnqueueSignal,
		MemoryPassInput{
			UserID: sess.UserID, SessionID: sessionID, TurnID: turnID,
			BatchSeqs: batchSeqs, ReferenceTime: time.Now().UTC(),
		}, client.StartWorkflowOptions{
			ID:                       "memory-pass-queue:" + sess.UserID,
			TaskQueue:                TaskQueue,
			WorkflowIDReusePolicy:    enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
			WorkflowIDConflictPolicy: enums.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
		}, MemoryPassQueueWorkflow, MemoryPassQueueInput{UserID: sess.UserID})
	return err
}

// toolFail 工具参数/用法错误：返回模型可见的错误 JSON（exit_code=-1），**不是** activity error——
// activity error 会被 Temporal 无限重试，校验错误重试无意义且会卡死 turn（M5 e2e 实测）。
// 只有基础设施错误（PG 挂等）才应作为 activity error 返回。
func toolFail(format string, args ...any) (string, error) {
	b, _ := json.Marshal(map[string]any{"exit_code": -1, "stderr": fmt.Sprintf(format, args...)})
	return string(b), nil
}

// heartbeat 长阻塞 activity 的取消通道：Temporal 只把取消请求投递给有心跳的活动
// （官方：Activities must heartbeat to receive cancellations），否则用户打断后
// 这个 HTTP 请求仍会跑满 StartToCloseTimeout。非 activity 上下文返回 no-op。
func heartbeat(ctx context.Context) (stop func()) {
	if !activity.IsActivity(ctx) {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				activity.RecordHeartbeat(ctx, nil)
			}
		}
	}()
	return func() { close(done) }
}

// warn 活动内用 Temporal logger；单测直接调用（无 activity 上下文）时退回标准日志。
func warn(ctx context.Context, msg string, args ...any) {
	if activity.IsActivity(ctx) {
		activity.GetLogger(ctx).Warn(msg, args...)
		return
	}
	log.Println(append([]any{msg}, args...)...)
}

// ===== M5 服务端记忆工具（spec Q6；不经 VM/agentd，第二条工具执行路径）=====

type ExecMemoryToolInput struct {
	SessionID string
	Name      string
	Input     string // JSON
}

// memoryHitOut memory_search 的输出行（正文截断，全文走 memory_read）。
type memoryHitOut struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	Text string `json:"text"`
}

// ExecMemoryTool 服务端执行 memory_* 工具：session → user → PG。
// 输出统一 JSON 字符串（与 shell 工具落库形状一致）。
func (a *Activities) ExecMemoryTool(ctx context.Context, in ExecMemoryToolInput) (string, error) {
	sid, err := uuid.Parse(in.SessionID)
	if err != nil {
		return "", err
	}
	sess, err := a.PG.GetSession(ctx, sid)
	if err != nil {
		return "", err
	}
	uid := sess.UserID

	switch in.Name {
	case "memory_search":
		var args struct {
			Query string `json:"query"`
			Limit int32  `json:"limit"`
		}
		if err := json.Unmarshal([]byte(in.Input), &args); err != nil || args.Query == "" {
			return toolFail("memory_search 参数非法: %s", in.Input)
		}
		if args.Limit <= 0 {
			args.Limit = 8
		}
		hits, err := a.PG.SearchMemories(ctx, uid, args.Query, args.Limit)
		if err != nil {
			return "", err
		}
		out := make([]memoryHitOut, 0, len(hits))
		for _, h := range hits {
			text := h.Text
			if len(text) > 500 { // 搜索结果截断，全文走 memory_read
				text = text[:500] + "…"
			}
			out = append(out, memoryHitOut{Kind: h.Kind, ID: h.ID.String(), Text: text})
		}
		b, _ := json.Marshal(map[string]any{"hits": out})
		return string(b), nil

	case "memory_read":
		var args struct {
			Kind string `json:"kind"`
			ID   string `json:"id"`
		}
		if err := json.Unmarshal([]byte(in.Input), &args); err != nil {
			return toolFail("memory_read 参数非法: %s", in.Input)
		}
		id, err := uuid.Parse(args.ID)
		if err != nil {
			return toolFail("memory_read 的 id 不是 uuid: %s", args.ID)
		}
		var b []byte
		switch args.Kind {
		case "memory":
			m, err := a.PG.GetMemoryByID(ctx, id)
			if errors.Is(err, pg.ErrNotFound) {
				return toolFail("记忆不存在（id 可能过期或幻觉）: %s", args.ID)
			}
			if err != nil {
				return "", err
			}
			b, _ = json.Marshal(map[string]any{"kind": "memory", "type": m.Type, "text": m.Text})
		case "skill":
			sk, err := a.PG.GetSkillByID(ctx, id)
			if errors.Is(err, pg.ErrNotFound) {
				return toolFail("技能不存在（id 可能过期或幻觉）: %s", args.ID)
			}
			if err != nil {
				return "", err
			}
			b, _ = json.Marshal(map[string]any{"kind": "skill", "name": sk.Name, "content": sk.Content})
		default:
			return toolFail("memory_read 的 kind 非法: %q", args.Kind)
		}
		return string(b), nil

	case "memory_save":
		var args struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			Importance  int32  `json:"importance"`
			TTLMinutes  *int32 `json:"ttl_minutes"`
			Name        string `json:"name"`
			Description string `json:"description"`
			Content     string `json:"content"`
		}
		if err := json.Unmarshal([]byte(in.Input), &args); err != nil {
			return toolFail("memory_save 参数非法: %s", in.Input)
		}
		if args.Importance == 0 {
			args.Importance = 5
		}
		if args.Type != "note" && args.TTLMinutes != nil {
			return toolFail("memory_save 仅 note 可设置 ttl_minutes: %s", in.Input)
		}
		if args.Type == "note" && (args.TTLMinutes == nil || *args.TTLMinutes <= 0) {
			return toolFail("memory_save note 需要正数 ttl_minutes: %s", in.Input)
		}
		op := pg.MemoryOp{Op: "add", Importance: args.Importance, TTLMinutes: args.TTLMinutes}
		if args.Type == "skill" {
			if args.Name == "" || args.Content == "" {
				return toolFail("memory_save skill 缺 name/content: %s", in.Input)
			}
			op.TargetType = "skill"
			op.Name, op.Description, op.Content = args.Name, args.Description, args.Content
		} else {
			if args.Text == "" || (args.Type != "profile" && args.Type != "log" && args.Type != "note") {
				return toolFail("memory_save 缺 text 或 type 非法: %s", in.Input)
			}
			op.TargetType = "memory"
			op.Type, op.Text = args.Type, args.Text
		}
		applied, err := a.PG.ApplyMemoryOps(ctx, uid, []pg.MemoryOp{op}, "agent")
		if err != nil {
			return "", err
		}
		b, _ := json.Marshal(map[string]any{"saved": true, "applied": applied})
		return string(b), nil

	default:
		return toolFail("unknown memory tool %q", in.Name)
	}
}
