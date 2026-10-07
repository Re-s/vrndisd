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
