package client

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"vrndis/internal/proto"
	"vrndis/internal/tap"
)

// fakeServer 内存服务器：accept 一条连接并执行 handler。
func fakeServer(t *testing.T, handler func(conn net.Conn)) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		handler(conn)
	}()
	return ln.Addr().String(), func() { ln.Close(); <-done }
}

// serverAssign 读取 HELLO 后下发 ASSIGN，并保持连接打开（直到对端关闭），
// 避免服务器主动关闭造成 EOF 与断言竞态。
func serverAssign(conn net.Conn) {
	_, err := proto.ReadMessage(conn) // 读 HELLO
	if err != nil {
		return
	}
	proto.WriteMessage(conn, proto.TypeAssign, mustJSON(proto.Assignment{
		Version:   proto.ProtocolVersion,
		LeaseID:   1,
		IP:        "10.77.0.2",
		Prefix:    24,
		Gateway:   "10.77.0.1",
		DNS:       []string{"8.8.8.8"},
		MTU:       1500,
		MAC:       "02:00:00:00:00:01",
		ServerMAC: "02:00:00:00:00:00",
		ServerIP:  "10.77.0.1",
	}))
	// 保持连接存活：持续读，直到对端关闭（返回错误即结束）。
	for {
		if _, err := proto.ReadMessage(conn); err != nil {
			return
		}
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func TestClientConnectAssignConfiguresDevice(t *testing.T) {
	dev := tap.NewFake("c0")
	addr, stop := fakeServer(t, serverAssign)
	defer stop()

	c := New(Config{
		Server:      addr,
		TapName:     "c0",
		Name:        "test",
		DialTimeout: 2 * time.Second,
		Device:      dev,
	})
	asg, err := c.Connect()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Stop()

	if asg.IP != "10.77.0.2" || asg.MAC != "02:00:00:00:00:01" {
		t.Fatalf("assign: %+v", asg)
	}
	if dev.IPv4 != "10.77.0.2/24" {
		t.Fatalf("device ipv4: %q", dev.IPv4)
	}
	if dev.MAC.String() != "02:00:00:00:00:01" {
		t.Fatalf("device mac: %v", dev.MAC)
	}
	if !dev.UpFlag {
		t.Fatal("device not up")
	}
	if got := c.Assignment(); got == nil || got.IP != "10.77.0.2" {
		t.Fatalf("Assignment(): %+v", got)
	}
	if c.Closed() {
		t.Fatal("should not be closed after connect")
	}
}

func TestClientConnectDialError(t *testing.T) {
	// 无监听端口
	c := New(Config{
		Server:      "127.0.0.1:1", // 大概率拒绝
		TapName:     "c1",
		DialTimeout: 500 * time.Millisecond,
	})
	if _, err := c.Connect(); err == nil {
		t.Fatal("dial to closed port should fail")
	}
}

func TestClientConnectNonAssignType(t *testing.T) {
	addr, stop := fakeServer(t, func(conn net.Conn) {
		proto.ReadMessage(conn)
		proto.WriteMessage(conn, proto.TypeHello, nil) // 错误类型
	})
	defer stop()
	c := New(Config{Server: addr, TapName: "c", DialTimeout: 2 * time.Second, Device: tap.NewFake("c")})
	if _, err := c.Connect(); err == nil {
		t.Fatal("expected non-ASSIGN to fail")
	}
}

func TestClientConnectBadAssignJSON(t *testing.T) {
	addr, stop := fakeServer(t, func(conn net.Conn) {
		proto.ReadMessage(conn)
		proto.WriteMessage(conn, proto.TypeAssign, []byte("not json"))
	})
	defer stop()
	c := New(Config{Server: addr, TapName: "c", DialTimeout: 2 * time.Second, Device: tap.NewFake("c")})
	if _, err := c.Connect(); err == nil {
		t.Fatal("expected bad JSON to fail")
	}
}

func TestClientConnectInvalidIP(t *testing.T) {
	addr, stop := fakeServer(t, func(conn net.Conn) {
		proto.ReadMessage(conn)
		proto.WriteMessage(conn, proto.TypeAssign, mustJSON(proto.Assignment{
			Version: 1, IP: "not-an-ip", MAC: "02:00:00:00:00:01",
		}))
	})
	defer stop()
	c := New(Config{Server: addr, TapName: "c", DialTimeout: 2 * time.Second, Device: tap.NewFake("c")})
	if _, err := c.Connect(); err == nil {
		t.Fatal("expected invalid IP to fail")
	}
}

func TestClientDataForwardedToDevice(t *testing.T) {
	dev := tap.NewFake("c")
	addr, stop := fakeServer(t, func(conn net.Conn) {
		proto.ReadMessage(conn) // HELLO
		proto.WriteMessage(conn, proto.TypeAssign, mustJSON(proto.Assignment{
			Version: 1, IP: "10.77.0.2", Prefix: 24, Gateway: "10.77.0.1",
			MAC: "02:00:00:00:00:01", ServerMAC: "02:00:00:00:00:00",
		}))
		proto.WriteMessage(conn, proto.TypeData, []byte("payload-frame"))
	})
	defer stop()

	c := New(Config{Server: addr, TapName: "c", DialTimeout: 2 * time.Second, Device: dev})
	if _, err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	defer c.Stop()

	// DATA 帧应写入设备
	got, err := dev.Capture()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "payload-frame" {
		t.Fatalf("device got %q", got)
	}
}

func TestClientRespondsToPing(t *testing.T) {
	dev := tap.NewFake("c")
	pongCh := make(chan struct{}, 1)
	addr, stop := fakeServer(t, func(conn net.Conn) {
		proto.ReadMessage(conn) // HELLO
		proto.WriteMessage(conn, proto.TypeAssign, mustJSON(proto.Assignment{
			Version: 1, IP: "10.77.0.2", Prefix: 24, Gateway: "10.77.0.1",
			MAC: "02:00:00:00:00:01", ServerMAC: "02:00:00:00:00:00",
		}))
		// 等客户端进入循环后再 PING
		time.Sleep(50 * time.Millisecond)
		proto.WriteMessage(conn, proto.TypePing, nil)
		msg, _ := proto.ReadMessage(conn)
		if msg.Type == proto.TypePong {
			pongCh <- struct{}{}
		}
	})
	defer stop()

	c := New(Config{Server: addr, TapName: "c", DialTimeout: 2 * time.Second, Device: dev})
	if _, err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	defer c.Stop()

	select {
	case <-pongCh:
	case <-time.After(2 * time.Second):
		t.Fatal("client did not answer PING with PONG")
	}
}

func TestClientByeTriggersStop(t *testing.T) {
	dev := tap.NewFake("c")
	addr, stop := fakeServer(t, func(conn net.Conn) {
		proto.ReadMessage(conn)
		proto.WriteMessage(conn, proto.TypeAssign, mustJSON(proto.Assignment{
			Version: 1, IP: "10.77.0.2", Prefix: 24, Gateway: "10.77.0.1",
			MAC: "02:00:00:00:00:01", ServerMAC: "02:00:00:00:00:00",
		}))
		time.Sleep(50 * time.Millisecond)
		proto.WriteMessage(conn, proto.TypeBye, nil)
	})
	defer stop()

	c := New(Config{Server: addr, TapName: "c", DialTimeout: 2 * time.Second, Device: dev})
	if _, err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	// BYE 应触发 teardown，最终 c.Closed() 为 true
	deadline := time.Now().Add(2 * time.Second)
	for !c.Closed() {
		if time.Now().After(deadline) {
			t.Fatal("BYE did not stop client")
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.Stop()
}

func TestClientRunStopsOnContextCancel(t *testing.T) {
	dev := tap.NewFake("c")
	addr, stop := fakeServer(t, serverAssign)
	defer stop()

	c := New(Config{Server: addr, TapName: "c", DialTimeout: 2 * time.Second, Device: dev})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	// 等待连接建立
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestClientDeviceSendToServer(t *testing.T) {
	dev := tap.NewFake("c")
	gotCh := make(chan string, 1)
	addr, stop := fakeServer(t, func(conn net.Conn) {
		proto.ReadMessage(conn) // HELLO
		proto.WriteMessage(conn, proto.TypeAssign, mustJSON(proto.Assignment{
			Version: 1, IP: "10.77.0.2", Prefix: 24, Gateway: "10.77.0.1",
			MAC: "02:00:00:00:00:01", ServerMAC: "02:00:00:00:00:00",
		}))
		msg, _ := proto.ReadMessage(conn) // 等待客户端网卡注入的 DATA
		if msg.Type == proto.TypeData {
			gotCh <- string(msg.Body)
		}
	})
	defer stop()

	c := New(Config{Server: addr, TapName: "c", DialTimeout: 2 * time.Second, Device: dev})
	if _, err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	defer c.Stop()

	time.Sleep(50 * time.Millisecond) // 等协程就绪
	dev.Inject([]byte("from-device"))
	select {
	case got := <-gotCh:
		if got != "from-device" {
			t.Fatalf("server got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not receive device frame")
	}
}
