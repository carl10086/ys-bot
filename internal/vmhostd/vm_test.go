package vmhostd

import "testing"

// reconcile 处置分类是纯决策逻辑（spec Q3：sleeping 跳过清场，快照丢失转 stopped）。
func TestClassifySandbox(t *testing.T) {
	cases := []struct {
		name       string
		status     string
		procAlive  bool
		hasSnap    bool
		wantAction reconcileAction
	}{
		{"awake 进程在 = 正常", "awake", true, false, reconcileKeep},
		{"awake 进程死 = 清场", "awake", false, false, reconcileClean},
		{"creating 进程死 = 清场", "creating", false, false, reconcileClean},
		{"sleeping 快照在 = 正常（没进程是休眠的本义）", "sleeping", false, true, reconcileKeep},
		{"sleeping 快照丢 = 转 stopped", "sleeping", false, false, reconcileOrphanStop},
		{"sleeping 进程还在 = 不动（休眠中途崩溃留给 resume 防御）", "sleeping", true, true, reconcileKeep},
		{"stopped 不动", "stopped", false, false, reconcileKeep},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifySandbox(c.status, c.procAlive, c.hasSnap); got != c.wantAction {
				t.Fatalf("classifySandbox(%q, proc=%v, snap=%v) = %v, want %v",
					c.status, c.procAlive, c.hasSnap, got, c.wantAction)
			}
		})
	}
}
