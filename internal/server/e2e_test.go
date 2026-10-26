// 端到端测试：真实 TCP 连接 + 内存网卡，覆盖完整数据面。
package server_test

import (
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Re-s/vrndisd/internal/client"
	"github.com/Re-s/vrndisd/internal/ethernet"
	"github.com/Re-s/vrndisd/internal/proto"
	"github.com/Re-s/vrndisd/internal/server"
	"github.com/Re-s/vrndisd/internal/tap"
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
		P2P:     true, // P2P 控制通道默认开启：不影响不发 P2P 消息的测试
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

// ---- 缺陷 A 回归：服务器重启后 MAC/IP 按 name 粘稳定 ----

func TestStickyLeaseAcrossServerRestart(t *testing.T) {
	leaseFile := filepath.Join(t.TempDir(), "leases.json")
	network, gateway := randTestNetwork()

	var clients []*client.Client
	connect := func(srv *server.Server, name string) *proto.Assignment {
		t.Helper()
		c := client.New(client.Config{
			Server: fmt.Sprintf("127.0.0.1:%d", srv.Port()),
			Name:   name,
			Device: tap.NewFake(name),
		})
		a, err := c.Connect()
		if err != nil {
			t.Fatalf("客户端 %s 连接失败: %v", name, err)
		}
		clients = append(clients, c)
		return a
	}
	stopAll := func(srv *server.Server) {
		for _, c := range clients {
			c.Stop()
		}
		clients = nil
		waitUntil(t, 2*time.Second, func() bool { return srv.SessionCount() == 0 })
	}

	mkServer := func() *server.Server {
		srv, err := server.New(server.Config{
			Listen:    "127.0.0.1:0",
			Network:   network,
			Gateway:   gateway,
			TapMAC:    "02:00:00:00:00:00",
			Device:    tap.NewFake("srv"),
			P2P:       true,
			LeaseFile: leaseFile,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := srv.Start(); err != nil {
			t.Fatal(err)
		}
		return srv
	}

	// 第一周期：devA/devB 顺序接入。
	srv := mkServer()
	a1 := connect(srv, "devA")
	b1 := connect(srv, "devB")
	if a1.IP == b1.IP || a1.MAC == b1.MAC {
		t.Fatalf("同周期地址不应重复: %+v %+v", a1, b1)
	}
	stopAll(srv)
	srv.Stop()

	// 第二周期：重启服务器，且故意反转接入顺序——
	// 无粘滞时 devB 会先拿到 devA 的 MAC（缺陷 A 现象）。
	srv = mkServer()
	defer srv.Stop()
	b2 := connect(srv, "devB")
	a2 := connect(srv, "devA")
	if a2.IP != a1.IP || a2.MAC != a1.MAC {
		t.Fatalf("devA 租约漂移: 旧 %s/%s 新 %s/%s", a1.IP, a1.MAC, a2.IP, a2.MAC)
	}
	if b2.IP != b1.IP || b2.MAC != b1.MAC {
		t.Fatalf("devB 租约漂移: 旧 %s/%s 新 %s/%s", b1.IP, b1.MAC, b2.IP, b2.MAC)
	}
	// 全新客户端仍走顺序分配。
	c2 := connect(srv, "devC")
	if c2.IP == a2.IP || c2.IP == b2.IP {
		t.Fatalf("新客户端不应复用占用中的地址: %s", c2.IP)
	}
}

// 同名并发连接：各拿不同 IP/MAC，服务器不崩、会话各自存活。
func TestSameNameConcurrentClients(t *testing.T) {
	leaseFile := filepath.Join(t.TempDir(), "leases.json")
	srvDev := tap.NewFake("srv")
	network, gateway := randTestNetwork()
	srv, err := server.New(server.Config{
		Listen:    "127.0.0.1:0",
		Network:   network,
		Gateway:   gateway,
		TapMAC:    "02:00:00:00:00:00",
		Device:    srvDev,
		P2P:       true,
		LeaseFile: leaseFile,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	connected := make(chan *proto.Assignment, 2)
	errs := make(chan error, 2)
	var mu sync.Mutex
	var conns []*client.Client
	for i := 0; i < 2; i++ {
		go func() {
			c := client.New(client.Config{
				Server: fmt.Sprintf("127.0.0.1:%d", srv.Port()),
				Name:   "same-name",
				Device: tap.NewFake("same"),
			})
			a, err := c.Connect()
			if err != nil {
				errs <- err
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			connected <- a
			select {} // 保持连接不断开
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	var assigns []*proto.Assignment
	for len(assigns) < 2 && time.Now().Before(deadline) {
		select {
		case a := <-connected:
			assigns = append(assigns, a)
		case err := <-errs:
			t.Fatalf("同名客户端连接失败: %v", err)
		case <-time.After(time.Until(deadline)):
		}
	}
	defer func() {
		mu.Lock()
		for _, c := range conns {
			go c.Stop()
		}
		mu.Unlock()
	}()
	if len(assigns) != 2 {
		t.Fatalf("应有两个同名会话，got %d", len(assigns))
	}
	if assigns[0].IP == assigns[1].IP || assigns[0].MAC == assigns[1].MAC {
		t.Fatalf("同名客户端地址重复: %+v vs %+v", assigns[0], assigns[1])
	}
	waitUntil(t, 2*time.Second, func() bool { return srv.SessionCount() == 2 })
}

// 加密（PSK）链路的完整连接周期。回归：分配租约前读取的首条 Hello 被解密后
// 在消息循环里遭到二次解密，导致每个 PSK 连接在 Assign 后立即被打死。
// 首条消息只解密一次；粘滞租约在加密链路上同样生效。
func TestPSKEncryptedSession(t *testing.T) {
	leaseFile := filepath.Join(t.TempDir(), "leases.json")
	srvDev := tap.NewFake("srv")
	network, gateway := randTestNetwork()
	mkServer := func() *server.Server {
		srv, err := server.New(server.Config{
			Listen:    "127.0.0.1:0",
			Network:   network,
			Gateway:   gateway,
			TapMAC:    "02:00:00:00:00:00",
			Device:    srvDev,
			P2P:       false,
			PSK:       []byte("e2e-psk"),
			LeaseFile: leaseFile,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := srv.Start(); err != nil {
			t.Fatal(err)
		}
		return srv
	}
	srv := mkServer()
	defer srv.Stop()

	addr := fmt.Sprintf("127.0.0.1:%d", srv.Port())
	dev := tap.NewFake("enc-a")
	c := client.New(client.Config{Server: addr, Name: "enc-a", Device: dev, PSK: []byte("e2e-psk")})
	defer c.Stop()
	a1, err := c.Connect()
	if err != nil {
		t.Fatalf("加密连接失败: %v", err)
	}
	// 数据面：客户端帧应到达服务器 TAP。
	frame := ethernet.BuildFrame(
		mustMAC(t, a1.ServerMAC), mustMAC(t, a1.MAC), ethernet.TypeIPv4, []byte("over-psk"))
	if err := dev.Inject(frame); err != nil {
		t.Fatal(err)
	}
	got := recvFrame(t, srvDev)
	if string(got) != string(frame) {
		t.Fatalf("帧不一致: %d vs %d 字节", len(got), len(frame))
	}
	// 服务器→客户端方向：广播帧应送达。
	bcast := ethernet.BuildFrame(
		ethernet.Broadcast, mustMAC(t, "02:00:00:00:00:00"), ethernet.TypeIPv4, []byte("down"))
	if err := srvDev.Inject(bcast); err != nil {
		t.Fatal(err)
	}
	down := recvFrameOn(t, dev)
	if string(down) != string(bcast) {
		t.Fatalf("下行帧不一致: %d vs %d 字节", len(down), len(bcast))
	}

	// 重启后同 name 复用原租约（加密链路上粘滞同样生效）。
	c.Stop()
	waitUntil(t, 2*time.Second, func() bool { return srv.SessionCount() == 0 })
	srv.Stop()
	srv2 := mkServer()
	defer srv2.Stop()
	addr2 := fmt.Sprintf("127.0.0.1:%d", srv2.Port())
	c2 := client.New(client.Config{Server: addr2, Name: "enc-a", Device: tap.NewFake("enc-a"), PSK: []byte("e2e-psk")})
	defer c2.Stop()
	a2, err := c2.Connect()
	if err != nil {
		t.Fatalf("重启后加密连接失败: %v", err)
	}
	if a2.IP != a1.IP || a2.MAC != a1.MAC {
		t.Fatalf("加密链路租约漂移: 旧 %s/%s 新 %s/%s", a1.IP, a1.MAC, a2.IP, a2.MAC)
	}
	// 错误 PSK 必须连接失败。
	bad := client.New(client.Config{Server: addr2, Name: "bad", Device: tap.NewFake("bad"), PSK: []byte("wrong")})
	if _, err := bad.Connect(); err == nil {
		bad.Stop()
		t.Fatal("错误 PSK 应连接失败")
	}
}

func recvFrameOn(t *testing.T, dev *tap.Fake) []byte {
	t.Helper()
	return recvFrame(t, dev)
}
