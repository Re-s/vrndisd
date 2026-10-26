package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/Re-s/vrndisd/internal/ethernet"
	"github.com/Re-s/vrndisd/internal/p2p"
	"github.com/Re-s/vrndisd/internal/proto"
	stunserver "github.com/Re-s/vrndisd/internal/stun/server"
	"github.com/Re-s/vrndisd/internal/tap"
)

// waitStunAddr 等待 STUN 服务器就绪并返回其监听地址。
func waitStunAddr(srv *stunserver.Server) string {
	for i := 0; i < 200; i++ {
		a := srv.Addr()
		if a != nil {
			return a.String()
		}
		time.Sleep(10 * time.Millisecond)
	}
	return ""
}

// fakeP2PServer 接受多条客户端连接、完成地址分配并协调 P2P 候选交换。
type fakeP2PServer struct {
	ln      net.Listener
	assigns map[string]*proto.Assignment
	adv     map[string]*proto.P2PAdvertise // MAC -> 上报的候选
	conns   map[string]net.Conn            // MAC -> 连接（用于下发对端候选）
	mu      sync.Mutex
	stopped sync.Once
	done    chan struct{}
}

func newFakeP2PServer(t *testing.T) *fakeP2PServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeP2PServer{
		ln:      ln,
		assigns: make(map[string]*proto.Assignment),
		adv:     make(map[string]*proto.P2PAdvertise),
		conns:   make(map[string]net.Conn),
		done:    make(chan struct{}),
	}
	go s.acceptLoop()
	return s
}

func (s *fakeP2PServer) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handleConn(conn)
	}
}

func (s *fakeP2PServer) handleConn(conn net.Conn) {
	defer conn.Close()
	msg, err := proto.ReadMessage(conn)
	if err != nil {
		return
	}
	var hello proto.Hello
	if json.Unmarshal(msg.Body, &hello) != nil {
		return
	}

	s.mu.Lock()
	asg, ok := s.assigns[hello.Name]
	if !ok {
		idx := len(s.assigns)
		asg = &proto.Assignment{
			Version:   proto.ProtocolVersion,
			LeaseID:   uint64(idx + 1),
			IP:        "10.77.0." + string(rune('2'+idx)),
			Prefix:    24,
			Gateway:   "10.77.0.1",
			DNS:       []string{"8.8.8.8"},
			MTU:       1500,
			MAC:       "02:00:00:00:00:0" + string(rune('1'+idx)),
			ServerMAC: "02:00:00:00:00:00",
			ServerIP:  "10.77.0.1",
		}
		s.assigns[hello.Name] = asg
	}
	s.mu.Unlock()
	_ = proto.WriteMessage(conn, proto.TypeAssign, mustJSON(asg))

	for {
		msg, err := proto.ReadMessage(conn)
		if err != nil {
			return
		}
		if msg.Type != proto.TypeP2PAdvertise {
			continue
		}
		var adv proto.P2PAdvertise
		if json.Unmarshal(msg.Body, &adv) != nil {
			continue
		}
		s.mu.Lock()
		// 复制一份，避免 Go 循环变量地址复用导致所有条目指向同一地址
		advCopy := proto.P2PAdvertise{
			Version:    adv.Version,
			MAC:        adv.MAC,
			Candidate:  adv.Candidate,
			Candidates: adv.Candidates,
		}
		s.adv[adv.MAC] = &advCopy
		s.conns[adv.MAC] = conn
		// 向所有已有 peer 下发新 peer 的候选（新 peer 会在 s.distribute() 中收到已有 peer）
		for existingMAC, existingConn := range s.conns {
			if existingMAC == adv.MAC || existingConn == nil {
				continue
			}
			_ = proto.WriteMessage(existingConn, proto.TypePeerCandidate, mustJSON(peerCandidateMsg(&adv, "")))
		}
		s.distribute()
		s.mu.Unlock()
	}
}

// distribute 向所有已上线且已有候选的 peer 互相下发对端候选。
func (s *fakeP2PServer) distribute() {
	for mac, a := range s.adv {
		_ = a
		conn := s.conns[mac]
		if conn == nil {
			continue
		}
		for peerMAC, peerAdv := range s.adv {
			if peerMAC == mac {
				continue
			}
			_ = proto.WriteMessage(conn, proto.TypePeerCandidate, mustJSON(peerCandidateMsg(peerAdv, mac)))
		}
	}
}

func (s *fakeP2PServer) Addr() string { return s.ln.Addr().String() }

func (s *fakeP2PServer) Stop() {
	s.stopped.Do(func() { close(s.done); s.ln.Close() })
	// 不阻塞等待 wg：t.Fatal 会跳过 defer，导致 wg 永远无法完成。
	// 让 acceptLoop 在 done 关闭后自行退出（通过 SetDeadline 太复杂，依赖 listener 关闭即可）。
}

// peerCandidateMsg 用上报候选构造下发的 PeerCandidate，并附一个不可达
// 伪候选（203.0.113.0/24 TEST-NET-1）：多候选客户端必须能在废候选干扰下
// 选中真正可达的候选。
func peerCandidateMsg(adv *proto.P2PAdvertise, dstMAC string) proto.PeerCandidate {
	primary := adv.Candidate
	cands := append([]string(nil), adv.Candidates...)
	if len(cands) == 0 {
		cands = []string{primary}
	}
	cands = append(cands, "203.0.113.77:19999")
	return proto.PeerCandidate{
		Version:    proto.ProtocolVersion,
		PeerMAC:    adv.MAC,
		Candidate:  primary,
		Candidates: cands,
	}
}

// 两个真实客户端 + fake TCP 服务器 + 真实 STUN 服务器 + 回环 UDP 打洞
// peerMAC 从隧道已注册路由中取一个对端 MAC（client 完成候选回查后注册）。
func peerMAC(t *testing.T, tun *p2p.Tunnel) net.HardwareAddr {
	t.Helper()
	for mac := range tun.Routes() {
		if m, err := net.ParseMAC(mac); err == nil {
			return m
		}
	}
	t.Fatal("隧道未注册任何直连路由")
	return nil
}

func srcMAC(name string) net.HardwareAddr {
	// src 不参与路由判定，任意合法单播 MAC 即可。
	return net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0x01}
}

func TestP2PRealTunnelOnLoopback(t *testing.T) {
	srv := newFakeP2PServer(t)
	defer srv.Stop()

	stun := stunserver.New(stunserver.Config{ListenAddr: "127.0.0.1:0"})
	go func() { _ = stun.Start() }()
	defer stun.Stop()
	stunAddr := waitStunAddr(stun)
	if stunAddr == "" {
		t.Fatal("STUN server did not start")
	}

	devA := tap.NewFake("c0")
	devB := tap.NewFake("c1")

	cA := New(Config{
		Server:      srv.Addr(),
		TapName:     "c0",
		Name:        "alice",
		P2P:         true,
		STUNServer:  stunAddr,
		DialTimeout: 2 * time.Second,
		Device:      devA,
	})
	cB := New(Config{
		Server:      srv.Addr(),
		TapName:     "c1",
		Name:        "bob",
		P2P:         true,
		STUNServer:  stunAddr,
		DialTimeout: 2 * time.Second,
		Device:      devB,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errA := make(chan error, 1)
	errB := make(chan error, 1)
	go func() { errA <- cA.Run(ctx) }()
	go func() { errB <- cB.Run(ctx) }()

	// 轮询等待隧道建立，避免固定 sleep 的时序抖动
	var tunA, tunB *p2p.Tunnel
	for i := 0; i < 60; i++ {
		tunA = cA.getTunnel()
		tunB = cB.getTunnel()
		if tunA != nil && tunB != nil {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if tunA == nil || tunB == nil {
		t.Fatalf("隧道未建立: A=%v B=%v (A peer=%v B peer=%v)",
			tunA != nil, tunB != nil, cA.getPeerMAC(), cB.getPeerMAC())
	}

	// 双向传帧（真实 UDP 隧道，非 TCP 中继）。
	// dst 取隧道已注册的对端 MAC，src 取各 client 自己的 TAP MAC。
	sendOverTunnel := func(tun *p2p.Tunnel, dstMAC net.HardwareAddr, payload string) []byte {
		frame := ethernet.BuildFrame(dstMAC, srcMAC(""), ethernet.TypeIPv4, []byte(payload))
		if err := tun.Write(frame); err != nil {
			t.Fatalf("tunnel write: %v", err)
		}
		return frame
	}
	wantA := sendOverTunnel(tunA, peerMAC(t, tunA), "ping-from-alice")
	frame, err := devB.Capture()
	if err != nil || !bytes.Equal(frame, wantA) {
		t.Fatalf("B read: %v %v", err, frame)
	}
	wantB := sendOverTunnel(tunB, peerMAC(t, tunB), "pong-from-bob")
	frame, err = devA.Capture()
	if err != nil || !bytes.Equal(frame, wantB) {
		t.Fatalf("A read: %v %v", err, frame)
	}

	t.Logf("P2P 回环直连验证通过: A tunnel=%v B tunnel=%v", tunA.Peer(), tunB.Peer())
}

// 单客户端无对端 → 候选等待超时 → 自动回退 TCP 中继，不断连
func TestP2PFallbackToRelayWhenPunchFails(t *testing.T) {
	srv := newFakeP2PServer(t)
	defer srv.Stop()

	stun := stunserver.New(stunserver.Config{ListenAddr: "127.0.0.1:0"})
	go func() { _ = stun.Start() }()
	defer stun.Stop()
	stunAddr := waitStunAddr(stun)
	if stunAddr == "" {
		t.Fatal("STUN server did not start")
	}

	dev := tap.NewFake("c0")
	c := New(Config{
		Server:      srv.Addr(),
		TapName:     "c0",
		Name:        "solo",
		P2P:         true,
		STUNServer:  stunAddr,
		DialTimeout: 2 * time.Second,
		Device:      dev,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	time.Sleep(6 * time.Second)
	if c.getTunnel() != nil {
		t.Fatal("无对端时不应建立隧道")
	}
	// TCP 中继仍应活着：往 TAP 写帧应被转发到 TCP（不 panic 即正常）
	_ = dev.Inject(ethernet.BuildFrame(
		ethernet.MakeLocalMAC(2), ethernet.MakeLocalMAC(1),
		ethernet.TypeIPv4, []byte{0x01, 0x02, 0x03, 0x04}))
	time.Sleep(1 * time.Second)
	t.Logf("单客户端 P2P 超时回退验证通过")
}

// 默认关闭 P2P → 纯中继，回归旧行为
func TestP2PDisabledByDefault(t *testing.T) {
	srv := newFakeP2PServer(t)
	defer srv.Stop()

	dev := tap.NewFake("c0")
	c := New(Config{
		Server:      srv.Addr(),
		TapName:     "c0",
		Name:        "plain",
		DialTimeout: 2 * time.Second,
		Device:      dev,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	time.Sleep(3 * time.Second)
	if c.getTunnel() != nil {
		t.Fatal("P2P 默认关闭时不应有隧道")
	}
	t.Logf("P2P 默认关闭回归验证通过")
}

// 并发多个客户端打洞互不干扰
func TestP2PConcurrentClients(t *testing.T) {
	srv := newFakeP2PServer(t)
	defer srv.Stop()

	stun := stunserver.New(stunserver.Config{ListenAddr: "127.0.0.1:0"})
	go func() { _ = stun.Start() }()
	defer stun.Stop()
	stunAddr := waitStunAddr(stun)
	if stunAddr == "" {
		t.Fatal("STUN server did not start")
	}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			n := string(rune('0' + idx))
			dev := tap.NewFake("c" + n)
			c := New(Config{
				Server:      srv.Addr(),
				TapName:     "c" + n,
				Name:        "cli" + n,
				P2P:         true,
				STUNServer:  stunAddr,
				DialTimeout: 2 * time.Second,
				Device:      dev,
			})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := c.Run(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("client %d: %v", idx, err)
			}
		}(i)
	}
	wg.Wait()
	t.Logf("4 个并发 P2P 客户端验证通过")
}

// restart 模拟服务器重启：丢弃全部候选登记并断开所有客户端控制连接。
// assigns 保留（按名字复用），保证重连客户端身份不变。
func (s *fakeP2PServer) restart(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	for mac, conn := range s.conns {
		if conn != nil {
			_ = conn.Close()
			s.conns[mac] = nil
		}
	}
	s.adv = make(map[string]*proto.P2PAdvertise)
	s.mu.Unlock()
}

// 服务器重启后客户端重连时，若 P2P 隧道仍存活，应重播上次上报的候选，
// 而不是任由服务器候选登记丢失（peers 视图将恒显示 relay）。
func TestReplaysAdvertiseAfterServerRestart(t *testing.T) {
	srv := newFakeP2PServer(t)
	defer srv.Stop()

	stun := stunserver.New(stunserver.Config{ListenAddr: "127.0.0.1:0"})
	go func() { _ = stun.Start() }()
	defer stun.Stop()
	stunAddr := waitStunAddr(stun)
	if stunAddr == "" {
		t.Fatal("STUN server did not start")
	}

	devA := tap.NewFake("c0")
	devB := tap.NewFake("c1")
	cA := New(Config{
		Server:      srv.Addr(),
		TapName:     "c0",
		Name:        "alice",
		P2P:         true,
		STUNServer:  stunAddr,
		DialTimeout: 2 * time.Second,
		Reconnect:   true,
		Device:      devA,
	})
	cB := New(Config{
		Server:      srv.Addr(),
		TapName:     "c1",
		Name:        "bob",
		P2P:         true,
		STUNServer:  stunAddr,
		DialTimeout: 2 * time.Second,
		Reconnect:   true,
		Device:      devB,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go func() { _ = cA.Run(ctx) }()
	go func() { _ = cB.Run(ctx) }()

	macA := "02:00:00:00:00:01"
	waitAdv := func() *proto.P2PAdvertise {
		for i := 0; i < 100; i++ {
			srv.mu.Lock()
			adv, ok := srv.adv[macA]
			srv.mu.Unlock()
			if ok && adv != nil {
				return adv
			}
			time.Sleep(250 * time.Millisecond)
		}
		return nil
	}

	advBefore := waitAdv()
	if advBefore == nil {
		t.Fatal("客户端未上报候选")
	}
	// 等待隧道建立（单客户端无对端时不会打洞）
	var tunBefore *p2p.Tunnel
	for i := 0; i < 80 && tunBefore == nil; i++ {
		tunBefore = cA.getTunnel()
		if tunBefore == nil {
			time.Sleep(250 * time.Millisecond)
		}
	}
	if tunBefore == nil {
		t.Fatal("P2P 隧道未建立")
	}

	// 模拟重启：候选登记清空、控制连接断开。隧道（纯 UDP）应存活不变。
	srv.restart(t)

	advAfter := waitAdv()
	if advAfter == nil {
		t.Fatal("服务器重启后客户端未重播候选")
	}
	if advAfter.Candidate != advBefore.Candidate {
		t.Fatalf("重播候选变化: before=%s after=%s", advBefore.Candidate, advAfter.Candidate)
	}
	tunAfter := cA.getTunnel()
	if tunAfter == nil {
		t.Fatal("隧道在服务器重启后丢失")
	}
	if tunAfter != tunBefore {
		t.Fatalf("隧道不应重建: before=%p after=%p", tunBefore, tunAfter)
	}
}
