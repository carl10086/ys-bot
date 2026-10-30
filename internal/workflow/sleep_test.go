package workflow

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/testsuite"
)

// sweep：有候选时调 StartSleepWorkflows，无候选时不调（不注册 mock，被调即失败）。
func TestSleepSweepWorkflow(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{})

	env.OnActivity("ListSleepCandidates", mock.Anything, int32(10)).Return([]string{"sb-1"}, nil)
	env.OnActivity("StartSleepWorkflows", mock.Anything, []string{"sb-1"}, int32(10)).Return(nil)

	env.ExecuteWorkflow(SleepSweepWorkflow, SleepSweepInput{IdleMinutes: 10})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	env.AssertExpectations(t)

	var suite2 testsuite.WorkflowTestSuite
	env2 := suite2.NewTestWorkflowEnvironment()
	env2.RegisterActivity(&Activities{})
	env2.OnActivity("ListSleepCandidates", mock.Anything, int32(10)).Return([]string{}, nil)

	env2.ExecuteWorkflow(SleepSweepWorkflow, SleepSweepInput{IdleMinutes: 10})
	if err := env2.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	env2.AssertExpectations(t)
}

// CAS 占坑（带过期复核）→ 抢不到直接收工 → 抢到才调 vmhostd SleepVM →
// SleepVM 失败必须回滚 awake（否则 VM 还醒着但状态卡在 sleeping，下次懒唤醒会走 resume 撞空）。

// M4 SleepWorkflow 的期望形状（spec Q2/Q3）：
// CAS 占坑（带过期复核）→ 抢不到直接收工 → 抢到才调 vmhostd SleepVM →
// SleepVM 失败必须回滚 awake（否则 VM 还醒着但状态卡在 sleeping，下次懒唤醒会走 resume 撞空）。
func TestSleepWorkflow_CASLosesDoesNothing(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{})

	env.OnActivity("TryStartSleep", mock.Anything, "sb-1", int32(10)).Return(false, nil)
	// SleepSandbox / RollbackSleep 未注册 mock——被调用即 unexpected call 失败

	env.ExecuteWorkflow(SleepWorkflow, SleepInput{SandboxID: "sb-1", IdleMinutes: 10})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("CAS 抢不到应安静收工: %v", err)
	}
	env.AssertExpectations(t)
}

func TestSleepWorkflow_HappyPath(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{})

	env.OnActivity("TryStartSleep", mock.Anything, "sb-1", int32(10)).Return(true, nil)
	env.OnActivity("SleepSandbox", mock.Anything, "sb-1").Return(nil)

	env.ExecuteWorkflow(SleepWorkflow, SleepInput{SandboxID: "sb-1", IdleMinutes: 10})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	env.AssertExpectations(t)
}

func TestSleepWorkflow_SleepFailsRollsBack(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{})

	env.OnActivity("TryStartSleep", mock.Anything, "sb-1", int32(10)).Return(true, nil)
	env.OnActivity("SleepSandbox", mock.Anything, "sb-1").Return(errors.New("fc pause failed"))
	env.OnActivity("RollbackSleep", mock.Anything, "sb-1").Return(nil)

	env.ExecuteWorkflow(SleepWorkflow, SleepInput{SandboxID: "sb-1", IdleMinutes: 10})
	if err := env.GetWorkflowError(); err == nil {
		t.Fatal("SleepSandbox 失败应使 workflow 返回错误（下轮 Schedule 再试）")
	}
	env.AssertExpectations(t) // RollbackSleep 必须被调到
}
