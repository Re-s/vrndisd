package client

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/Re-s/vrndisd/internal/proto"
	"github.com/Re-s/vrndisd/internal/tap"
)

// fakeServerMulti 接受 n 条连接，依次对每条连接执行 handler(t, conn, idx)。
func fakeServerMulti(t *testing.T, n int, handler func(t *testing.T, conn net.Conn, idx int)) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < n; i++ {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn, idx int) {
				defer c.Close()
				handler(t, c, idx)
			}(conn, i)
		}
	}()
	return ln.Addr().String(), func() { ln.Close(); <-done }
}

// assignFor 生成第 idx 条连接的 ASSIGN；不同连接用不同 IP/MAC（模拟地址池重分配）。
func assignFor(idx int) proto.Assignment {
	ip := "10.77.0.2"
	mac := "02:00:00:00:00:01"
	switch idx {
	case 1:
		ip, mac = "10.77.0.3", "02:00:00:00:00:02"
	case 2:
		ip, mac = "10.77.0.4", "02:00:00:00:00:03"
	}
	return proto.Assignment{
		Version:   proto.ProtocolVersion,
		LeaseID:   uint64(idx + 1),
		IP:        ip,
		Prefix:    24,
		Gateway:   "10.77.0.1",
		DNS:       []string{"8.8.8.8"},
		MTU:       1500,
		MAC:       mac,
		ServerMAC: "02:00:00:00:00:00",
		ServerIP:  "10.77.0.1",
	}
}

// TestReconnectAfterEOF 模拟服务器读 HELLO 发 ASSIGN 后立刻关闭连接（EOF），
// 客户端应自动重连并重新握手、刷新网卡 IP/MAC。
func TestReconnectAfterEOF(t *testing.T) {
	dev := tap.NewFake("rc-eof")
	addr, stop := fakeServerMulti(t, 2, func(t *testing.T, conn net.Conn, idx int) {
		proto.ReadMessage(conn) // HELLO
		proto.WriteMessage(conn, proto.TypeAssign, mustJSON(assignFor(idx)))
		if idx == 0 {
			// 发一帧后立刻断开 → 客户端读连接得到 EOF。
			proto.WriteMessage(conn, proto.TypeData, []byte("before-drop"))
			return
		}
		// 第二条连接保持存活，等待客户端。
		time.Sleep(2 * time.Second)
	})
	defer stop()

	c := New(Config{
		Server:           addr,
		TapName:          "rc-eof",
		DialTimeout:      2 * time.Second,
		Reconnect:        true,
		ReconnectBackoff: 50 * time.Millisecond,
		Device:           dev,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	// 等待重连完成：设备上刷新为第二条连接的地址。
	deadline := time.Now().Add(5 * time.Second)
	for dev.IPv4Addr() != "10.77.0.3/24" || dev.MACAddr().String() != "02:00:00:00:00:02" {
		if time.Now().After(deadline) {
			t.Fatalf("reconnect did not refresh device; ipv4=%q mac=%v", dev.IPv4Addr(), dev.MACAddr())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if c.Closed() {
		t.Fatal("should be connected after reconnect")
	}
	if asg := c.Assignment(); asg == nil || asg.IP != "10.77.0.3" {
		t.Fatalf("assignment after reconnect: %+v", asg)
	}

	c.Stop()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after Stop")
	}
}

// TestReconnectAfterHeartbeatTimeout 模拟服务器发 ASSIGN 后保持 socket 打开但不再回应
// PING（半开/静默断网），客户端应靠心跳超时主动判定断线并重连。
func TestReconnectAfterHeartbeatTimeout(t *testing.T) {
	dev := tap.NewFake("rc-hb")
	addr, stop := fakeServerMulti(t, 2, func(t *testing.T, conn net.Conn, idx int) {
		proto.ReadMessage(conn) // HELLO
		proto.WriteMessage(conn, proto.TypeAssign, mustJSON(assignFor(idx)))
		// 不回应任何 PING，让客户端心跳超时。
		time.Sleep(2 * time.Second)
	})
	defer stop()

	c := New(Config{
		Server:            addr,
		TapName:           "rc-hb",
		DialTimeout:       2 * time.Second,
		Reconnect:         true,
		ReconnectBackoff:  50 * time.Millisecond,
		HeartbeatInterval: 100 * time.Millisecond,
		HeartbeatTimeout:  300 * time.Millisecond,
		Device:            dev,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for dev.IPv4Addr() != "10.77.0.3/24" {
		if time.Now().After(deadline) {
			t.Fatalf("heartbeat did not trigger reconnect; ipv4=%q", dev.IPv4Addr())
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.Stop()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after Stop")
	}
}

// TestReconnectMaxRetriesExceeded 服务器不可达且重连次数达到上限，Run 应返回错误。
func TestReconnectMaxRetriesExceeded(t *testing.T) {
	c := New(Config{
		Server:           "127.0.0.1:1", // 大概率拒绝
		TapName:          "rc-max",
		DialTimeout:      300 * time.Millisecond,
		Reconnect:        true,
		MaxRetries:       2,
		ReconnectBackoff: 10 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := c.Run(ctx); err == nil {
		t.Fatal("expected error after exceeding max retries")
	}
	if !c.Closed() {
		t.Fatal("expected client closed after giving up")
	}
}

// TestReconnectStopsOnContextCancel 无限重连期间 ctx 取消应能停止客户端。
func TestReconnectStopsOnContextCancel(t *testing.T) {
	c := New(Config{
		Server:           "127.0.0.1:1",
		TapName:          "rc-ctx",
		DialTimeout:      200 * time.Millisecond,
		Reconnect:        true,
		ReconnectBackoff: 20 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	time.Sleep(250 * time.Millisecond) // 让它连续重连几次
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop on context cancel")
	}
}
