package vmhostd

import (
	"testing"
)

func TestSlotAddrs(t *testing.T) {
	cases := []struct {
		slot                int32
		hostIP, guestIP     string
		guestCIDR, tap, mac string
	}{
		{0, "10.200.0.1/30", "10.200.0.2", "10.200.0.2/30", "tap0", "06:00:AC:10:00:00"},
		{3, "10.200.0.13/30", "10.200.0.14", "10.200.0.14/30", "tap3", "06:00:AC:10:00:03"},
		{255, "10.200.3.253/30", "10.200.3.254", "10.200.3.254/30", "tap255", "06:00:AC:10:00:FF"},
	}
	for _, c := range cases {
		a := slotAddrs(c.slot)
		if a.hostIP != c.hostIP || a.guestIP != c.guestIP || a.guestCIDR != c.guestCIDR || a.tap != c.tap || a.mac != c.mac {
			t.Errorf("slot %d: got %+v", c.slot, a)
		}
	}
}

func TestMinFreeSlot(t *testing.T) {
	cases := []struct {
		used []int32
		want int32
	}{
		{nil, 0},
		{[]int32{0}, 1},
		{[]int32{0, 1, 2}, 3},
		{[]int32{1, 2}, 0}, // 回收复用：0 空出来先给 0
		{[]int32{0, 2, 3}, 1},
	}
	for _, c := range cases {
		if got := minFreeSlot(c.used, maxSlots); got != c.want {
			t.Errorf("minFreeSlot(%v) = %d, want %d", c.used, got, c.want)
		}
	}
}

func TestMinFreeSlot_Full(t *testing.T) {
	used := make([]int32, maxSlots)
	for i := range used {
		used[i] = int32(i)
	}
	if got := minFreeSlot(used, maxSlots); got != -1 {
		t.Fatalf("full pool should return -1, got %d", got)
	}
}
