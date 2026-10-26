package addr

import (
	"errors"
	"net"
	"testing"
)

func TestAllocateReleaseReuse(t *testing.T) {
	p, err := New("10.77.0.0/29", "10.77.0.1") // 可用 .2-.6 共 5 个
	if err != nil {
		t.Fatal(err)
	}
	if p.Free() != 5 {
		t.Fatalf("free=%d", p.Free())
	}
	var got []string
	for i := 0; i < 5; i++ {
		l, err := p.Allocate()
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, l.IP.String())
	}
	want := []string{"10.77.0.2", "10.77.0.3", "10.77.0.4", "10.77.0.5", "10.77.0.6"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("allocate order: got %v want %v", got, want)
		}
	}
	if _, err := p.Allocate(); !errors.Is(err, ErrExhausted) {
		t.Fatalf("want ErrExhausted, got %v", err)
	}
	// 释放后应优先复用
	p.Release(net.ParseIP("10.77.0.3"))
	l, err := p.Allocate()
	if err != nil || l.IP.String() != "10.77.0.3" {
		t.Fatalf("reuse failed: %v %v", l, err)
	}
	if p.InUse() != 5 {
		t.Fatalf("in-use=%d", p.InUse())
	}
}

func TestLeaseFields(t *testing.T) {
	p, _ := New("10.77.0.0/24", "10.77.0.1")
	l, err := p.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if l.IP.String() != "10.77.0.2" || l.Prefix != 24 {
		t.Fatalf("lease: %+v", l)
	}
	if l.MAC.String() != "02:00:00:00:00:01" {
		t.Fatalf("mac: %s", l.MAC)
	}
	if l.Gateway.String() != "10.77.0.1" {
		t.Fatalf("gw: %s", l.Gateway)
	}
}

func TestGatewayExcluded(t *testing.T) {
	p, _ := New("10.77.0.0/29", "10.77.0.1")
	for i := 0; i < 5; i++ {
		l, err := p.Allocate()
		if err != nil {
			t.Fatal(err)
		}
		if l.IP.Equal(net.ParseIP("10.77.0.1")) || l.IP.Equal(net.ParseIP("10.77.0.0")) ||
			l.IP.Equal(net.ParseIP("10.77.0.7")) {
			t.Fatalf("分配了保留地址: %s", l.IP)
		}
	}
}

func TestInvalidConfig(t *testing.T) {
	if _, err := New("10.77.0.0/24", "192.168.1.1"); err == nil {
		t.Fatal("gateway outside network must fail")
	}
	if _, err := New("not-a-cidr", "10.77.0.1"); err == nil {
		t.Fatal("bad cidr must fail")
	}
	if _, err := New("2001:db8::/64", "2001:db8::1"); err == nil {
		t.Fatal("IPv6 must be rejected")
	}
}

func TestDoubleReleaseSafe(t *testing.T) {
	p, _ := New("10.77.0.0/29", "10.77.0.1")
	l, _ := p.Allocate()
	p.Release(l.IP)
	p.Release(l.IP) // 二次释放不得使池膨胀
	if p.Free() != 5 {
		t.Fatalf("free=%d after double release", p.Free())
	}
}

// AllocateSpecific 复用原租约：同 IP+MAC 重分配成功，且不与后续 Allocate 冲突。
func TestAllocateSpecific(t *testing.T) {
	p, err := New("10.77.0.0/24", "10.77.0.1")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mac := net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0x07}
	ip := net.IPv4(10, 77, 0, 7)
	l, err := p.AllocateSpecific(ip, mac)
	if err != nil {
		t.Fatalf("AllocateSpecific: %v", err)
	}
	if !l.IP.Equal(ip) || l.MAC.String() != mac.String() {
		t.Fatalf("租约内容错误: ip=%v mac=%v", l.IP, l.MAC)
	}
	// 池计数正确
	if p.InUse() != 1 || p.Free() != 252 {
		t.Fatalf("计数错误: inuse=%d free=%d", p.InUse(), p.Free())
	}
	// 再分配一个普通租约：IP 顺序绕过被复用的 .7，MAC 也不与复用的冲突
	l2, err := p.Allocate()
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if l2.IP.Equal(ip) {
		t.Fatal("普通分配拿到了被复用的 IP")
	}
	if l2.MAC.String() == mac.String() {
		t.Fatal("普通分配生成了与复用租约相同的 MAC")
	}
	// 释放后按 name 粘滞的场景由 server 层覆盖；此处仅验证占用冲突
	if _, err := p.AllocateSpecific(ip, mac); err == nil {
		t.Fatal("占用中的地址不应再次分配成功")
	}
	// 释放后可再次指定分配
	p.Release(l.IP)
	if _, err := p.AllocateSpecific(ip, mac); err != nil {
		t.Fatalf("释放后复用失败: %v", err)
	}
}

// 非法入参：不在池内 / 是网关 / 畸形 MAC。
func TestAllocateSpecificInvalid(t *testing.T) {
	p, err := New("10.77.0.0/24", "10.77.0.1")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mac := net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0x09}
	badMAC := net.HardwareAddr{0x02, 0x00}
	for _, c := range []struct {
		name string
		ip   net.IP
		mac  net.HardwareAddr
	}{
		{"外网段", net.IPv4(10, 77, 1, 9), mac},
		{"网关", net.IPv4(10, 77, 0, 1), mac},
		{"畸形 MAC", net.IPv4(10, 77, 0, 9), badMAC},
	} {
		if _, err := p.AllocateSpecific(c.ip, c.mac); err == nil {
			t.Fatalf("%s: 应当失败", c.name)
		}
	}
}
