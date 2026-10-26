package client

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/Re-s/vrndisd/internal/crypto"
	"github.com/Re-s/vrndisd/internal/proto"
	"github.com/Re-s/vrndisd/internal/tap"
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
	if dev.IPv4Addr() != "10.77.0.2/24" {
		t.Fatalf("device ipv4: %q", dev.IPv4Addr())
	}
	if dev.MACAddr().String() != "02:00:00:00:00:01" {
		t.Fatalf("device mac: %v", dev.MACAddr())
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

// writeEnvelope 服务器侧：把 [真实类型]+[真实body] 加密成信封消息发出。
func writeEnvelope(t *testing.T, conn net.Conn, sec *crypto.SecureConn, typ byte, body []byte) {
	t.Helper()
	plain := append([]byte{typ}, body...)
	cipher, err := sec.Encrypt(plain)
	if err != nil {
		t.Fatal(err)
	}
	if err := proto.WriteMessage(conn, proto.TypeData, cipher); err != nil {
		t.Fatal(err)
	}
}

// readEnvelope 服务器侧：读取并解密客户端发来的信封消息，返回真实类型与 body。
func readEnvelope(t *testing.T, conn net.Conn, sec *crypto.SecureConn) (byte, []byte) {
	t.Helper()
	msg, err := proto.ReadMessage(conn)
	if err != nil {
		t.Fatalf("read envelope: %v", err)
	}
	plain, err := sec.Decrypt(msg.Body)
	if err != nil {
		t.Fatalf("decrypt envelope: %v", err)
	}
	if len(plain) == 0 {
		t.Fatal("empty envelope")
	}
	return plain[0], plain[1:]
}

// TestClientEncryptedPingDispatch 回归：加密信封模式下，控制消息（PING，空 body）
// 必须被正确 dispatch（应答 PONG），不得被当作数据帧写入网卡——
// 修复前 sockToDev 在解密前判断外层类型，空 body 的 PING 经 dev.Write 触发
// Linux TUN EINVAL 导致连接被关闭。
func TestClientEncryptedPingDispatch(t *testing.T) {
	dev := tap.NewFake("c")
	pongCh := make(chan struct{}, 1)
	addr, stop := fakeServer(t, func(conn net.Conn) {
		sec, err := crypto.ServerHandshake(conn, []byte("psk-regress"), proto.ProtocolVersion)
		if err != nil {
			return
		}
		// 读 HELLO（信封）
		if typ, _ := readEnvelope(t, conn, sec); typ != proto.TypeHello {
			t.Errorf("期望 HELLO，收到 %s", proto.TypeName(typ))
		}
		// 下发 ASSIGN（信封）
		writeEnvelope(t, conn, sec, proto.TypeAssign, mustJSON(proto.Assignment{
			Version: 1, IP: "10.77.0.2", Prefix: 24, Gateway: "10.77.0.1",
			MAC: "02:00:00:00:00:01", ServerMAC: "02:00:00:00:00:00",
		}))
		time.Sleep(80 * time.Millisecond)
		// 发 PING（空 body）——修复前会触发客户端 dev.Write(0 字节)
		writeEnvelope(t, conn, sec, proto.TypePing, nil)
		// 读应答：解包后应为 PONG
		if typ, body := readEnvelope(t, conn, sec); typ == proto.TypePong && len(body) == 0 {
			pongCh <- struct{}{}
		}
		// 保持连接存活，让客户端跑够心跳间隔
		for {
			if _, err := proto.ReadMessage(conn); err != nil {
				return
			}
		}
	})
	defer stop()

	c := New(Config{
		Server:      addr,
		PSK:         []byte("psk-regress"),
		TapName:     "c",
		DialTimeout: 2 * time.Second,
		Device:      dev,
	})
	if _, err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	defer c.Stop()

	select {
	case <-pongCh:
	case <-time.After(3 * time.Second):
		t.Fatal("加密模式下客户端未应答 PONG（控制消息被误当数据帧）")
	}
	// 等待足够时间确认客户端未因空帧断线；此时连接应仍然存活。
	time.Sleep(300 * time.Millisecond)
	if c.Closed() {
		t.Fatal("连接已断开（PING 被误写网卡导致 EINVAL）")
	}
}

// protectedFake 包装 Fake，记录默认路由调用参数。
type protectedFake struct {
	*tap.Fake
	gw       net.IP
	exclude  net.IP
	protCall int
	plainCal int
}

func (p *protectedFake) AddDefaultRouteProtected(gateway, exclude net.IP) error {
	p.gw, p.exclude, p.protCall = gateway, exclude, p.protCall+1
	return nil
}

func TestClientDefaultRouteExcludesServer(t *testing.T) {
	dev := &protectedFake{Fake: tap.NewFake("c-dr")}
	addr, stop := fakeServer(t, serverAssign)
	defer stop()
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	c := New(Config{
		Server:       addr,
		TapName:      "c-dr",
		Name:         "test",
		DefaultRoute: true,
		DialTimeout:  2 * time.Second,
		Device:       dev, // 包装设备实现 AddDefaultRouteProtected
	})
	if _, err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	defer c.Stop()

	// 默认路由须走保护路径（exclude = 服务器地址）。
	if dev.protCall != 1 {
		t.Fatalf("AddDefaultRouteProtected 调用次数 = %d，want 1", dev.protCall)
	}
	if !dev.gw.Equal(net.ParseIP("10.77.0.1")) {
		t.Fatalf("gateway = %v，want 10.77.0.1", dev.gw)
	}
	if !dev.exclude.Equal(net.ParseIP(host)) {
		t.Fatalf("exclude = %v，want %s", dev.exclude, host)
	}
}

func TestClientNoDefaultRouteSkipsProtection(t *testing.T) {
	dev := &protectedFake{Fake: tap.NewFake("c-nodr")}
	addr, stop := fakeServer(t, serverAssign)
	defer stop()
	c := New(Config{
		Server:       addr,
		TapName:      "c-nodr",
		Name:         "test",
		DefaultRoute: false,
		DialTimeout:  2 * time.Second,
		Device:       dev,
	})
	if _, err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	defer c.Stop()
	if dev.protCall != 0 {
		t.Fatalf("未启用 default route 仍调用了 AddDefaultRouteProtected (%d 次)", dev.protCall)
	}
}
