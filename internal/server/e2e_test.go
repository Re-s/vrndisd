// 端到端测试：真实 TCP 连接 + 内存网卡，覆盖完整数据面。
package server_test

import (
	"fmt"
	"net"
	"testing"
	"time"

	"vrndis/internal/client"
	"vrndis/internal/ethernet"
	"vrndis/internal/proto"
	"vrndis/internal/server"
	"vrndis/internal/tap"
)

// ---- 测试脚手架 ----

type stack struct {
	t       *testing.T
	srv     *server.Server
	srvDev  *tap.Fake
	clients []*client.Client
	devs    []*tap.Fake
	gateway string // 测试网段网关（随机生成）
}

func newStack(t *testing.T) *stack {
	t.Helper()
	srvDev := tap.NewFake("srv")
	network, gateway := randTestNetwork()
	srv, err := server.New(server.Config{
		Listen:  "127.0.0.1:0",
		Network: network,
		Gateway: gateway,
		TapMAC:  "02:00:00:00:00:00",
		Device:  srvDev,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	s := &stack{t: t, srv: srv, srvDev: srvDev, gateway: gateway}
	t.Cleanup(func() {
		for _, c := range s.clients {
			c.Stop()
		}
		srv.Stop()
	})
	return s
}

func (s *stack) connect(name string) (*client.Client, *tap.Fake, *proto.Assignment) {
	s.t.Helper()
	dev := tap.NewFake(name)
	c := client.New(client.Config{
		Server: fmt.Sprintf("127.0.0.1:%d", s.srv.Port()),
		Name:   name,
		Device: dev,
	})
	assign, err := c.Connect()
	if err != nil {
		s.t.Fatalf("客户端 %s 连接失败: %v", name, err)
	}
	s.clients = append(s.clients, c)
	s.devs = append(s.devs, dev)
	return c, dev, assign
}

func recvFrame(t *testing.T, dev *tap.Fake) []byte {
	t.Helper()
	type res struct {
		frame []byte
		err   error
	}
	ch := make(chan res, 1)
	go func() {
		f, err := dev.Capture()
		ch <- res{f, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("capture: %v", r.err)
		}
		return r.frame
	case <-time.After(2 * time.Second):
		t.Fatal("等待帧超时")
	}
	return nil
}

func expectNoFrame(t *testing.T, dev *tap.Fake, d time.Duration) {
	t.Helper()
	go func() { _, _ = dev.Capture() }() // 吞掉可能迟到的帧，避免阻塞后续
	time.Sleep(d)
}

func waitUntil(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("条件未在期限内满足")
}

// ---- 端到端用例 ----

func TestAssignmentFromServer(t *testing.T) {
	s := newStack(t)
	_, dev, a := s.connect("cli")
	if a.IP != firstClientIP(s.gateway) || a.Prefix != 24 || a.Gateway != s.gateway {
		t.Fatalf("assignment: %+v", a)
	}
	if a.MAC != "02:00:00:00:00:01" || a.ServerMAC != "02:00:00:00:00:00" {
		t.Fatalf("macs: %+v", a)
	}
	if a.MTU != 1500 || len(a.DNS) == 0 {
		t.Fatalf("mtu/dns: %+v", a)
	}
	if dev.MAC.String() != a.MAC || dev.IPv4 != firstClientIP(s.gateway)+"/24" || !dev.UpFlag {
		t.Fatalf("本地网卡配置错误: %+v", dev)
	}
}

func TestClientToServerFrame(t *testing.T) {
	s := newStack(t)
	_, dev, a := s.connect("cli")
	frame := ethernet.BuildFrame(
		mustMAC(t, a.ServerMAC), mustMAC(t, a.MAC), ethernet.TypeIPv4, []byte("hello-server"))
	if err := dev.Inject(frame); err != nil {
		t.Fatal(err)
	}
	got := recvFrame(t, s.srvDev)
	if string(got) != string(frame) {
		t.Fatalf("帧不一致: %d vs %d 字节", len(got), len(frame))
	}
}

func TestServerToClientFrame(t *testing.T) {
	s := newStack(t)
	_, dev, a := s.connect("cli")
	frame := ethernet.BuildFrame(
		mustMAC(t, a.MAC), mustMAC(t, a.ServerMAC), ethernet.TypeIPv4, []byte("hello-client"))
	if err := s.srvDev.Inject(frame); err != nil {
		t.Fatal(err)
	}
	got := recvFrame(t, dev)
	if string(got) != string(frame) {
		t.Fatal("帧不一致")
	}
}

func TestBroadcastReachesAllClients(t *testing.T) {
	s := newStack(t)
	_, dev1, a1 := s.connect("c1")
	_, dev2, a2 := s.connect("c2")
	_ = a1
	_ = a2
	frame := ethernet.BuildFrame(
		ethernet.Broadcast, mustMAC(t, "02:00:00:00:00:00"), ethernet.TypeARP, []byte("who"))
	if err := s.srvDev.Inject(frame); err != nil {
		t.Fatal(err)
	}
	if string(recvFrame(t, dev1)) != string(frame) || string(recvFrame(t, dev2)) != string(frame) {
		t.Fatal("广播未到达全部客户端")
	}
}

func TestClientToClientForwarding(t *testing.T) {
	s := newStack(t)
	_, dev1, a1 := s.connect("c1")
	_, dev2, a2 := s.connect("c2")
	if a1.IP == a2.IP {
		t.Fatal("地址应不同")
	}
	frame := ethernet.BuildFrame(
		mustMAC(t, a2.MAC), mustMAC(t, a1.MAC), ethernet.TypeIPv4, []byte("p2p"))
	if err := dev1.Inject(frame); err != nil {
		t.Fatal(err)
	}
	if string(recvFrame(t, dev2)) != string(frame) {
		t.Fatal("P2P 转发失败")
	}
}

func TestARPHandshakeOverTunnel(t *testing.T) {
	s := newStack(t)
	_, dev, a := s.connect("cli")
	req := ethernet.ARPRequest(
		mustMAC(t, a.MAC), net.ParseIP(a.IP).To4(), net.ParseIP(a.Gateway).To4())
	if err := dev.Inject(req); err != nil {
		t.Fatal(err)
	}
	got := recvFrame(t, s.srvDev)
	arp, err := ethernet.ParseARP(payloadOf(t, got))
	if err != nil {
		t.Fatal(err)
	}
	if arp.TPA.String() != s.gateway || arp.SPA.String() != a.IP {
		t.Fatalf("ARP 请求内容: %+v", arp)
	}
	reply := ethernet.ARPReply(
		mustMAC(t, a.ServerMAC), net.ParseIP(a.Gateway).To4(),
		mustMAC(t, a.MAC), net.ParseIP(a.IP).To4())
	if err := s.srvDev.Inject(reply); err != nil {
		t.Fatal(err)
	}
	got = recvFrame(t, dev)
	arp, err = ethernet.ParseARP(payloadOf(t, got))
	if err != nil {
		t.Fatal(err)
	}
	if arp.Op != ethernet.ARPOpReply || arp.SHA.String() != a.ServerMAC {
		t.Fatalf("ARP 应答: %+v", arp)
	}
}

func TestLeaseReleasedOnDisconnect(t *testing.T) {
	s := newStack(t)
	c1, _, a1 := s.connect("c1")
	if a1.IP != firstClientIP(s.gateway) {
		t.Fatalf("first lease: %s", a1.IP)
	}
	c1.Stop()
	waitUntil(t, 2*time.Second, func() bool { return s.srv.SessionCount() == 0 })
	_, _, a2 := s.connect("c2")
	if a2.IP != firstClientIP(s.gateway) {
		t.Fatalf("释放的地址应复用，got %s", a2.IP)
	}
}

func TestMultipleClientsDistinctAddresses(t *testing.T) {
	s := newStack(t)
	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		_, _, a := s.connect(fmt.Sprintf("bulk%d", i))
		if seen[a.IP] {
			t.Fatalf("地址重复: %s", a.IP)
		}
		seen[a.IP] = true
	}
}

// ---- 工具 ----

func mustMAC(t *testing.T, s string) net.HardwareAddr {
	t.Helper()
	mac, err := ethernet.MACFromString(s)
	if err != nil {
		t.Fatal(err)
	}
	return mac
}

func payloadOf(t *testing.T, frame []byte) []byte {
	t.Helper()
	f, err := ethernet.ParseFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	return f.Payload
}
