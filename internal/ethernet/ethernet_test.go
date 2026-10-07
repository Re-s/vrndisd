package ethernet

import (
	"bytes"
	"net"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	dst := Broadcast
	src := MakeLocalMAC(7)
	frame := BuildFrame(dst, src, TypeIPv4, []byte("hello"))
	f, err := ParseFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(f.Dst, dst) || !bytes.Equal(f.Src, src) || f.EtherType != TypeIPv4 {
		t.Fatalf("parse mismatch: %+v", f)
	}
	if string(f.Payload) != "hello" {
		t.Fatalf("payload %q", f.Payload)
	}
}

func TestShortFrameRejected(t *testing.T) {
	if _, err := ParseFrame(make([]byte, 13)); err == nil {
		t.Fatal("short frame must be rejected")
	}
}

func TestMACHelpers(t *testing.T) {
	mac := MakeLocalMAC(2)
	if mac.String() != "02:00:00:00:00:02" {
		t.Fatalf("got %s", mac)
	}
	if IsMulticast(MakeLocalMAC(1)) {
		t.Fatal("local MAC must not be multicast")
	}
	if !IsMulticast(Broadcast) {
		t.Fatal("broadcast must be multicast")
	}
	// 索引 0 保留给服务器（02:00:00:00:00:00），客户端从 01 开始
	if MakeLocalMAC(0).String() != "02:00:00:00:00:00" {
		t.Fatalf("server mac: %s", MakeLocalMAC(0))
	}
	if MakeLocalMAC(1).String() != "02:00:00:00:00:01" {
		t.Fatalf("first client: %s", MakeLocalMAC(1))
	}
	if _, err := MACFromString("bad:mac"); err == nil {
		t.Fatal("bad MAC must be rejected")
	}
}

func TestARPRequestReply(t *testing.T) {
	sha := MakeLocalMAC(3)
	spa := net.ParseIP("10.77.0.2").To4()
	tpa := net.ParseIP("10.77.0.1").To4()

	req := ARPRequest(sha, spa, tpa)
	f, err := ParseFrame(req)
	if err != nil {
		t.Fatal(err)
	}
	if f.EtherType != TypeARP || !bytes.Equal(f.Dst, Broadcast) {
		t.Fatalf("bad request frame: %+v", f)
	}
	arp, err := ParseARP(f.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if arp.Op != ARPOpRequest || arp.SPA.String() != "10.77.0.2" || arp.TPA.String() != "10.77.0.1" {
		t.Fatalf("bad ARP: %+v", arp)
	}

	reply := ARPReply(MakeLocalMAC(0), tpa, sha, spa)
	f, err = ParseFrame(reply)
	if err != nil {
		t.Fatal(err)
	}
	arp, err = ParseARP(f.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if arp.Op != ARPOpReply || arp.SHA.String() != "02:00:00:00:00:00" {
		t.Fatalf("bad reply: %+v", arp)
	}
}

func TestBadARPTolerated(t *testing.T) {
	if _, err := ParseARP(make([]byte, 10)); err == nil {
		t.Fatal("short ARP must fail")
	}
	// 非以太网/IPv4 组合必须拒绝
	payload := make([]byte, 28)
	payload[4], payload[5] = 6, 4
	if _, err := ParseARP(payload); err == nil {
		t.Fatal("unsupported ARP flavor must fail")
	}
}

// FuzzParseFrame 模糊测试：任意字节不得 panic。
func FuzzParseFrame(f *testing.F) {
	f.Add(BuildFrame(Broadcast, MakeLocalMAC(1), TypeIPv4, []byte("x")))
	f.Add([]byte{})
	f.Add(make([]byte, 14))
	f.Fuzz(func(t *testing.T, data []byte) {
		fr, err := ParseFrame(data)
		if err != nil {
			return
		}
		if len(fr.Dst) != 6 || len(fr.Src) != 6 {
			t.Fatalf("bad MAC lengths: %d %d", len(fr.Dst), len(fr.Src))
		}
	})
}
