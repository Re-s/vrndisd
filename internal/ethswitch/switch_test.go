package ethswitch

import (
	"net"
	"testing"

	"vrndis/internal/ethernet"
)

func TestLearningAndForwarding(t *testing.T) {
	sw := New()
	macA := ethernet.MakeLocalMAC(10)
	macB := ethernet.MakeLocalMAC(11)
	all := map[string]bool{"port-a": true, "port-b": true, "tap": true}

	// 未知单播：泛洪（除入端口）
	out := sw.Forward(macB, "port-a", all)
	if !out["port-b"] || !out["tap"] || out["port-a"] {
		t.Fatalf("unknown unicast flood wrong: %v", out)
	}
	// 广播：泛洪
	out = sw.Forward(ethernet.Broadcast, "port-a", all)
	if !out["port-b"] || !out["tap"] {
		t.Fatalf("broadcast flood wrong: %v", out)
	}

	sw.Learn(macA, "port-a")
	sw.Learn(macB, "port-b")
	if out := sw.Forward(macB, "port-a", all); len(out) != 1 || !out["port-b"] {
		t.Fatalf("known unicast wrong: %v", out)
	}
	if out := sw.Forward(macA, "port-a", all); len(out) != 0 {
		t.Fatalf("self frame should not forward: %v", out)
	}

	sw.Forget("port-b")
	if _, ok := sw.Lookup(macB); ok {
		t.Fatal("forgotten MAC should be gone")
	}
	if sw.Size() != 1 {
		t.Fatalf("table size=%d", sw.Size())
	}
}

func TestMulticastFloodsAll(t *testing.T) {
	sw := New()
	mac := ethernet.MakeLocalMAC(1)
	sw.Learn(mac, "p1")
	dst := net.HardwareAddr{0x01, 0x00, 0x5e, 0x00, 0x00, 0x01} // IPv4 组播
	out := sw.Forward(dst, "p1", map[string]bool{"p1": true, "p2": true, "tap": true})
	if !out["p2"] || !out["tap"] {
		t.Fatalf("multicast must flood: %v", out)
	}
}
