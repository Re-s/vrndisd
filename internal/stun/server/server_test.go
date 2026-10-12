package server

import (
	"net"
	"testing"
	"time"
)

// TestSTUNServerNew 测试服务器创建
func TestSTUNServerNew(t *testing.T) {
	cfg := Config{
		ListenAddr: "127.0.0.1:0",
		RealmName:  "test",
	}
	srv := New(cfg)

	if srv.cfg.ListenAddr != cfg.ListenAddr {
		t.Errorf("ListenAddr = %q, want %q", srv.cfg.ListenAddr, cfg.ListenAddr)
	}
	if srv.cfg.RealmName != cfg.RealmName {
		t.Errorf("RealmName = %q, want %q", srv.cfg.RealmName, cfg.RealmName)
	}
}

// TestSTUNServerStartStop 测试服务器启动和停止
func TestSTUNServerStartStop(t *testing.T) {
	cfg := Config{
		ListenAddr: "127.0.0.1:0",
	}
	srv := New(cfg)

	go func() {
		if err := srv.Start(); err != nil {
			t.Errorf("Start failed: %v", err)
		}
	}()
	defer srv.Stop()

	// 给一点时间启动
	time.Sleep(100 * time.Millisecond)

	// 验证服务器端口已绑定
	if srv.udpConn == nil {
		t.Fatal("udpConn is nil")
	}
}

// TestXORFunction 测试 XOR 加密（内联实现）
func TestXORFunction(t *testing.T) {
	ip := net.ParseIP("192.168.1.100").To4()
	port := 12345
	if ip == nil {
		t.Fatal("Failed to parse IP")
	}

	encXOR := func(ip net.IP, port int) []byte {
		ip4 := ip.To4()
		if ip4 == nil {
			return nil
		}
		addr := make([]byte, 8)
		addr[0] = 1
		port2 := uint16(port)
		// 简单 XOR
		addr[1] = byte(port2 >> 8)
		addr[2] = byte(port2)
		addr[3] = 0
		addr[4] = ip4[0]
		addr[5] = ip4[1]
		addr[6] = ip4[2]
		addr[7] = ip4[3]
		key := []byte{0x21, 0x12, 0xA4, 0x42}
		for i := 0; i < 8; i++ {
			addr[i] ^= key[i%4]
		}
		return addr
	}

	encoded := encXOR(ip, port)
	if len(encoded) != 8 {
		t.Fatalf("Encoded length = %d, want 8", len(encoded))
	}
}

// TestSTUNServerStartReturnsAfterStop 验证 Stop 后 Start 能返回（优雅关停循环），
// 这是 server --stun 双服务同进程整体关停的基础。
func TestSTUNServerStartReturnsAfterStop(t *testing.T) {
	cfg := Config{ListenAddr: "127.0.0.1:0"}
	srv := New(cfg)
	done := make(chan error, 1)
	go func() { done <- srv.Start() }()

	deadline := time.Now().Add(1 * time.Second)
	for srv.udpConn == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if srv.udpConn == nil {
		t.Fatal("server did not bind")
	}

	srv.Stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned err=%v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after Stop（优雅关停被破坏）")
	}

	// Stop 幂等：再次调用不 panic。
	srv.Stop()
}
