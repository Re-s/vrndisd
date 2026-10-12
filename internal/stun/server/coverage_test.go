package server

import (
	"encoding/binary"
	"net"
	"testing"
	"time"

	"vrndis/internal/stun/protocol"
)

// TestEncodeXORIPv4 验证真实 encodeXOR：能解回原始 IP/端口。
func TestEncodeXORIPv4(t *testing.T) {
	ip := net.ParseIP("192.168.1.100").To4()
	got := encodeXOR(ip, 12345)
	if len(got) != 8 {
		t.Fatalf("len=%d want 8", len(got))
	}
	// 按 RFC 5389 §15.2 解码：family/保留字节不异或，端口与 key[0:2] 异或，
	// 地址与 key[0:4] 异或。
	key := []byte{0x21, 0x12, 0xA4, 0x42}
	if got[0] != 1 {
		t.Fatalf("family=%d want 1", got[0])
	}
	xorPort := binary.BigEndian.Uint16(got[1:3])
	port := int(xorPort ^ binary.BigEndian.Uint16(key[0:2]))
	var addr [4]byte
	for i := 0; i < 4; i++ {
		addr[i] = got[4+i] ^ key[i%4]
	}
	if port != 12345 {
		t.Fatalf("port=%d want 12345", port)
	}
	if !net.IP(addr[:]).Equal(ip) {
		t.Fatalf("ip=%v want %v", net.IP(addr[:]), ip)
	}
}

// TestEncodeXORIPv6Nil 非 IPv4 返回 nil。
func TestEncodeXORIPv6Nil(t *testing.T) {
	ip := net.ParseIP("2001:db8::1")
	if got := encodeXOR(ip, 1); got != nil {
		t.Fatalf("IPv6 should yield nil, got %v", got)
	}
	if got := encodeXOR(nil, 1); got != nil {
		t.Fatalf("nil should yield nil")
	}
}

// TestBindingRoundTrip 通过真实 UDP 发 Binding Request，应收到 Binding Response。
func TestBindingRoundTrip(t *testing.T) {
	cfg := Config{ListenAddr: "127.0.0.1:0"}
	srv := New(cfg)
	go func() {
		_ = srv.Start()
	}()
	defer srv.Stop()

	deadline := time.Now().Add(1 * time.Second)
	for srv.udpConn == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if srv.udpConn == nil {
		t.Fatal("server did not bind")
	}
	addr := srv.udpConn.LocalAddr().String()

	c, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	req := make([]byte, 20)
	req[0] = 0x00
	req[1] = 0x01
	copy(req[4:8], []byte{0x21, 0x12, 0xA4, 0x42})
	for i := 0; i < 12; i++ {
		req[8+i] = byte(i)
	}
	if _, err := c.Write(req); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	resp := make([]byte, 128)
	n, err := c.Read(resp)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := protocol.Parse(resp[:n])
	if err != nil {
		t.Fatal(err)
	}
	if msg.Type != protocol.BindingResponse {
		t.Fatalf("got type %v want BindingResponse", msg.Type)
	}
}
