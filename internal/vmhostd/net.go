// 网络寻址推导（纯函数，可测）+ tap 设备操作（exec ip 命令）。
// 模型砍自 e2b orchestrator/pkg/sandbox/network 的 slot 概念：
// 每 VM 一个 slot → 一个 /30 网段 → 宿主 tap 拿 .1，guest 拿 .2。
package vmhostd

import (
	"fmt"
	"os/exec"
	"sort"
)

// maxSlots slot 池大小（每 slot 一个 /30；256 个对单用户学习项目足够）。
const maxSlots = 256

// addrs 一个 slot 推导出的全部网络参数。
type addrs struct {
	hostIP    string // 宿主 tap 侧，带掩码："10.200.0.1/30"
	guestIP   string // guest 侧裸 IP："10.200.0.2"
	guestCIDR string // guest 侧带掩码："10.200.0.2/30"
	gateway   string // guest 的默认网关 = 宿主侧裸 IP："10.200.0.1"
	tap       string // tap 设备名："tap3"
	mac       string // guest MAC
}

// slotAddrs slot N → 第 N 个 /30 块（基址 = N*4，落在 10.200.0.0/16 内）。
func slotAddrs(slot int32) addrs {
	base := slot * 4
	octet := func(off int32) string {
		v := base + off
		return fmt.Sprintf("10.200.%d.%d", v>>8, v&0xff)
	}
	return addrs{
		hostIP:    octet(1) + "/30",
		guestIP:   octet(2),
		guestCIDR: octet(2) + "/30",
		gateway:   octet(1),
		tap:       fmt.Sprintf("tap%d", slot),
		mac:       fmt.Sprintf("06:00:AC:10:%02X:%02X", slot>>8&0xff, slot&0xff),
	}
}

// minFreeSlot 已占用集合里找最小空号；满了返回 -1。slot 要回收复用（计数器只增不减不适用）。
func minFreeSlot(used []int32, max int32) int32 {
	sort.Slice(used, func(i, j int) bool { return used[i] < used[j] })
	next := int32(0)
	for _, s := range used {
		if s > next {
			break
		}
		if s == next {
			next++
		}
	}
	if next >= max {
		return -1
	}
	return next
}

// createTap 建 tap 设备并配上宿主侧地址（幂等：先删后建）。
func createTap(a addrs) error {
	_ = exec.Command("ip", "link", "del", a.tap).Run() // 不存在则忽略
	for _, args := range [][]string{
		{"tuntap", "add", "dev", a.tap, "mode", "tap"},
		{"addr", "replace", a.hostIP, "dev", a.tap},
		{"link", "set", a.tap, "up"},
	} {
		if out, err := exec.Command("ip", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("ip %v: %w (%s)", args, err, out)
		}
	}
	return nil
}

// deleteTap 拆 tap（不存在不算错）。
func deleteTap(tap string) error {
	_ = exec.Command("ip", "link", "del", tap).Run()
	return nil
}
