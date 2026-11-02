package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/google/uuid"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"ys-sandbox/internal/agent"
	"ys-sandbox/internal/store/pg"
	"ys-sandbox/internal/store/redis"
	"ys-sandbox/internal/vmdriver"
)

// Activities TurnWorkflow 的全部副作用。每步落库 / 空信号 = 讲座 15 黑条的实现。
type Activities struct {
	PG          *pg.PG
	Redis       *redis.Redis
	Agent       agent.Agent
	Driver      vmdriver.Driver // nil = 无工具环境（M1 行为）
	TC          client.Client   // M4：sweep 从 activity 里起 SleepWorkflow（sleep.go 头注释有原因）
	MemoryAgent agent.Agent     // M5：提取专用 LLM 实例（System=ExtractPrompt）；nil = 记忆功能关闭
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
func (a *Activities) LoadHistory(ctx context.Context, sessionID string) ([]agent.Message, error) {
	sid, err := uuid.Parse(sessionID)
	if err != nil {
		return nil, err
	}
	msgs, err := a.PG.ListMessages(ctx, sid, 0)
	if err != nil {
		return nil, err
	}
	return projectHistory(msgs), nil
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

func (a *Activities) CallLLM(ctx context.Context, history []agent.Message) (agent.StepResult, error) {
	var tools []agent.ToolDef
	if a.Driver != nil { // 有 VM 环境才给工具（懒唤醒红线：纯对话不碰 VM）
		tools = []agent.ToolDef{shellTool}
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
		return "", fmt.Errorf("unknown tool %q", in.Name)
	}
	sbID, err := uuid.Parse(in.SandboxID)
	if err != nil {
		return "", err
	}
	var args struct {
		Cmd string `json:"cmd"`
	}
	if err := json.Unmarshal([]byte(in.Input), &args); err != nil || args.Cmd == "" {
		return "", fmt.Errorf("shell 工具参数非法: %s", in.Input)
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

func (a *Activities) WriteMessage(ctx context.Context, in WriteMessageInput) (int64, error) {
	sid, err := uuid.Parse(in.SessionID)
	if err != nil {
		return 0, err
	}
	content := in.Content
	if content == nil {
		content = pg.MarshalText(in.Text)
	}
	seq, _, err := a.PG.InsertMessage(ctx, pg.Message{
		SessionID: sid, Kind: in.Kind, Content: content, TurnID: in.TurnID,
	})
	return seq, err
}

func (a *Activities) PublishChanged(ctx context.Context, sessionID string) error {
	return a.Redis.PublishChanged(ctx, sessionID)
}

func (a *Activities) ReleaseTurn(ctx context.Context, sessionID, workflowID string) error {
	return a.Redis.ReleaseTurn(ctx, sessionID, workflowID)
}
