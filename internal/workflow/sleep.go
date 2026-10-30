// M4 快照休眠（spec: docs/ys-powers/specs/2026-10-30-m4-snapshot-sleep-design.md）。
// SleepSweepWorkflow：Temporal Schedule 每分钟点火，扫出过期 awake sandbox，逐个起 SleepWorkflow。
// SleepWorkflow：CAS 占坑（never-mid-turn 互斥核心）→ vmhostd SleepVM → 失败回滚 awake。
//
// 注意：SleepWorkflow 的启动走 activity 里的 client.ExecuteWorkflow，不用 child workflow——
// 实测（2026-10-30 e2e）：父 workflow 在同一 workflow task 里"发起 child + 立即完成"时，
// child 在 server 侧从未真正创建（Initiated 事件有，Started/Failed 都没有，Describe 查无此行）。
// client 启动与 hitsend 起 turn 同一模式，可靠且错误可见。
package workflow

import (
	"fmt"
	"time"

	"go.temporal.io/sdk/workflow"
)

// SleepSweepInput sweeper 的入参。IdleMinutes = 休眠阈值 N（spec 硬约束：N > 单工具 timeout 60s）。
type SleepSweepInput struct {
	IdleMinutes int32
}

// SleepInput 单个 sandbox 的休眠入参。
type SleepInput struct {
	SandboxID   string
	IdleMinutes int32
}

// SleepSweepWorkflow 全局扫描（跨宿主；路由信息在 sandbox.host_id 里，SleepVM 经 vmdriver 解析）。
func SleepSweepWorkflow(ctx workflow.Context, in SleepSweepInput) error {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: 30 * time.Second})
	var ids []string
	if err := workflow.ExecuteActivity(ctx, "ListSleepCandidates", in.IdleMinutes).Get(ctx, &ids); err != nil {
		return fmt.Errorf("list sleep candidates: %w", err)
	}
	if len(ids) > 0 {
		if err := workflow.ExecuteActivity(ctx, "StartSleepWorkflows", ids, in.IdleMinutes).Get(ctx, nil); err != nil {
			return fmt.Errorf("start sleep workflows: %w", err)
		}
	}
	workflow.GetLogger(ctx).Info("sleep sweep done", "candidates", len(ids))
	return nil
}

// SleepWorkflow 单个 sandbox 的休眠：CAS → SleepVM → 失败回滚。
// CAS 抢不到（刚有活动/已在睡/非 awake）= 安静收工，下轮 Schedule 再说。
func SleepWorkflow(ctx workflow.Context, in SleepInput) error {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: 2 * time.Minute})
	var acquired bool
	if err := workflow.ExecuteActivity(ctx, "TryStartSleep", in.SandboxID, in.IdleMinutes).Get(ctx, &acquired); err != nil {
		return fmt.Errorf("try start sleep: %w", err)
	}
	if !acquired {
		return nil
	}
	if err := workflow.ExecuteActivity(ctx, "SleepSandbox", in.SandboxID).Get(ctx, nil); err != nil {
		// 回滚 awake：VM 还醒着，不能让它卡在 sleeping（否则下次懒唤醒走 resume 撞空目录）
		_ = workflow.ExecuteActivity(ctx, "RollbackSleep", in.SandboxID).Get(ctx, nil)
		return fmt.Errorf("sleep sandbox: %w", err)
	}
	return nil
}
