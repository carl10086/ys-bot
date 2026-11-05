package workflow

import (
	"testing"
	"time"
)

// 重试策略是安全约束，不是调参：
//   - Temporal 默认策略是无限重试，一个必然失败的 turn 会永久占着会话锁；
//   - shell 有副作用且 agentd 没有幂等键，重试等于重跑命令；
//   - CallLLM 长阻塞，没有心跳就收不到取消（官方：Activities must heartbeat
//     to receive cancellations）。
func TestTurnActivityPolicies(t *testing.T) {
	if aoExec.RetryPolicy == nil || aoExec.RetryPolicy.MaximumAttempts != 1 {
		t.Fatalf("ExecTool 无幂等键，MaximumAttempts 必须为 1，got %+v", aoExec.RetryPolicy)
	}
	if aoRetry.RetryPolicy == nil || aoRetry.RetryPolicy.MaximumAttempts <= 0 {
		t.Fatalf("可重试 activity 必须显式给出最大尝试次数，got %+v", aoRetry.RetryPolicy)
	}
	if aoRetry.ScheduleToCloseTimeout <= 0 {
		t.Fatal("可重试 activity 必须有 ScheduleToCloseTimeout 兜住总时长")
	}
	// 会话锁 TTL = 600s：总时长超过它，锁会在 turn 结束前过期，并发 turn 就会互踩。
	if aoRetry.ScheduleToCloseTimeout >= 600*time.Second {
		t.Fatalf("ScheduleToCloseTimeout 必须短于会话锁 TTL(600s)，got %v", aoRetry.ScheduleToCloseTimeout)
	}
	if aoLLM.HeartbeatTimeout <= 0 {
		t.Fatal("CallLLM 必须设置 HeartbeatTimeout，否则取消请求到不了正在跑的 activity")
	}
	if aoLLM.StartToCloseTimeout <= aoLLM.HeartbeatTimeout {
		t.Fatal("StartToClose 必须大于心跳超时，否则 activity 会在两次心跳之间被判超时")
	}
}
