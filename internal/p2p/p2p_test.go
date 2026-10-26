package p2p

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Re-s/vrndisd/internal/ethernet"
	"time"

	stunserver "github.com/Re-s/vrndisd/internal/stun/server"
)

func mustUDPAddr(t *testing.T, host string, port int) *net.UDPAddr {
	t.Helper()
	a, err := net.ResolveUDPAddr("udp", net.JoinHostPort(host, itoa(port)))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return a
}

// 两个对端同时打洞：都应成功，随后可双向传帧。
func TestPunchBidirectional(t *testing.T) {
	tunA, tunB, macA, macB := punchPair(t)
	defer tunA.Close()
	defer tunB.Close()

	if tunA.Peer() == nil || tunB.Peer() == nil {
		t.Fatal("tunnel has no peer")
	}

	// 双向传帧（按目的 MAC 路由）。
	send := func(tun *Tunnel, dst, src net.HardwareAddr, payload string) []byte {
		t.Helper()
		frame := ethernet.BuildFrame(dst, src, ethernet.TypeIPv4, []byte(payload))
		if err := tun.Write(frame); err != nil {
			t.Fatalf("write: %v", err)
		}
		return frame
	}
	frameA := send(tunA, macB, macA, "ping-from-A")
	tunB.SetReadDeadline(time.Now().Add(1 * time.Second))
	gotB, err := tunB.Read()
	if err != nil {
		t.Fatalf("B read: %v", err)
	}
	if !bytes.Equal(gotB, frameA) {
		t.Fatalf("B read %v, want frame %v", gotB, frameA)
	}
	frameB := ethernet.BuildFrame(macA, macB, ethernet.TypeIPv4, []byte("pong-from-B"))
	if err := tunB.Write(frameB); err != nil {
		t.Fatalf("B write: %v", err)
	}
	tunA.SetReadDeadline(time.Now().Add(1 * time.Second))
	gotA, err := tunA.Read()
	if err != nil {
		t.Fatalf("A read: %v", err)
	}
	if !bytes.Equal(gotA, frameB) {
		t.Fatalf("A read %v, want frame %v", gotA, frameB)
	}
}

// 保活探测帧必须被数据面 Read 吸收，不污染 L2 数据流。
// 回归：此前 keepalive 探测与数据共用同一 socket，读到的真实数据帧之前可能先读到对端保活探测。
func TestReadAbsorbsKeepaliveProbe(t *testing.T) {
	tunA, tunB, _, _ := punchPair(t)
	defer tunA.Close()
	defer tunB.Close()

	// 用独立 socket 模拟对端直接向 A 的隧道端口注入：
	// 先一个保活探测帧，随后一个真实以太网帧。
	src, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		t.Fatalf("bind src: %v", err)
	}
	defer src.Close()
	aAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: tunA.Addr().(*net.UDPAddr).Port}

	if _, err := src.WriteToUDP(probeMagic, aAddr); err != nil {
		t.Fatalf("src write probe: %v", err)
	}
	// 确保探测帧先于数据帧到达 A。
	time.Sleep(50 * time.Millisecond)
	frame := ethernet.BuildFrame(net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0x99},
		net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0x98}, ethernet.TypeIPv4,
		[]byte("real-data-after-probe"))
	if _, err := src.WriteToUDP(frame, aAddr); err != nil {
		t.Fatalf("src write data: %v", err)
	}

	tunA.SetReadDeadline(time.Now().Add(1 * time.Second))
	got, err := tunA.Read()
	if err != nil {
		t.Fatalf("A read: %v", err)
	}
	if !bytes.Equal(got, frame) {
		t.Fatalf("A read %v, want data frame (probe must be absorbed)", got)
	}
}

// 回归：P2P 探测包乒乓风暴。
// 旧实现 Tunnel.Read 收到探测帧会无条件原路回一个探测帧，双方互答回包
// 形成无速率限制的自激回路（A 回 B -> B 收到又回 A -> ...），实测双向
// ~27k pps、6µs 背靠背包间隔、99.8% 空转流量。本测试模拟"狂暴对端"
// （如旧版本节点或异常/恶意来源）向 A 背靠背狂发 400 个探测帧，断言 A
// 的回复被按来源限速压到个位数，而不是每包必回。
func TestProbeReplyRateLimited(t *testing.T) {
	tunA, tunB, _, _ := punchPair(t)
	defer tunA.Close()
	defer tunB.Close()

	// A 必须 Read 才会吸收探测帧并（限速）回复。
	go func() {
		for {
			tunA.SetReadDeadline(time.Now().Add(3 * time.Second))
			if _, err := tunA.Read(); err != nil {
				return
			}
		}
	}()

	spoof, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		t.Fatalf("bind spoof: %v", err)
	}
	defer spoof.Close()
	// Addr() 在双栈监听上报 "::"，udp4 socket 无法向其发包；显式构造 IPv4 地址。
	aAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: tunA.Addr().(*net.UDPAddr).Port}

	// 背靠背狂发 400 个探测帧（无等待、无节流），模拟自激/攻击来源。
	for i := 0; i < 400; i++ {
		if _, err := spoof.WriteToUDP(probeMagic, aAddr); err != nil {
			t.Fatalf("spoof write: %v", err)
		}
	}

	// 统计 1.5 秒窗口内收到的回复。
	spoof.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
	got := 0
	buf := make([]byte, 64)
	for {
		n, _, err := spoof.ReadFromUDP(buf)
		if err != nil {
			break
		}
		if n == len(probeMagic) {
			got++
		}
	}
	// 1.5s 窗口、probeReplyMinInterval=1s：理论上限 ~2；放宽到 4 容忍毛刺，
	// 但必须远小于 400（否则就是每包必回的风扇）。
	if got > 4 {
		t.Fatalf("probe reply storm: got %d replies in 1.5s, want <=4", got)
	}
	t.Logf("rate-limited: %d replies for 400 probes", got)
}

// 回归：keepalive 周期必须是秒级，且本侧有真实数据发送时跳过保活探测。
// 旧实现 500ms 固定周期叠加乒乓回复，是空转流量的主要来源。
func TestKeepaliveSlowAndSkippedOnTraffic(t *testing.T) {
	tunA, tunB, macA, macB := punchPair(t)
	defer tunA.Close()
	defer tunB.Close()

	// 在 B 侧用计数器收 A 的探测帧：B 的 Read 会吸收它们。
	probeCount := int32(0)
	go func() {
		for {
			tunB.SetReadDeadline(time.Now().Add(2 * time.Second))
			frame, err := tunB.Read()
			if err != nil {
				return
			}
			if isProbe(frame) {
				atomic.AddInt32(&probeCount, 1)
			}
		}
	}()

	// 持续发送真实数据：有流量时 A 应跳过 keepalive 探测。
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				frame := ethernet.BuildFrame(macB, macA, ethernet.TypeIPv4, []byte("payload"))
				_ = tunA.Write(frame)
			}
		}
	}()

	time.Sleep(3 * time.Second)
	close(stop)

	// 3 秒内 keepaliveInterval=2s：即便完全不限速也只应发 ~1 个探测；
	// 有数据发送时还应进一步跳过。给宽松上限 4 防止边界毛刺。
	if n := atomic.LoadInt32(&probeCount); n > 4 {
		t.Fatalf("too many keepalive probes during traffic: %d in 3s, want <=4", n)
	}
}

// 打洞收集窗口内所有回包对端都应被收录进同一条隧道（单 socket 多对端）。
// 回归此前"首个回包者独占隧道、其余能直连的 peer 也被迫走中继"的问题。
func TestPunchCollectsMultiplePeers(t *testing.T) {

	conns := make([]*net.UDPConn, 3)
	for i := range conns {
		c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
		if err != nil {
			t.Fatalf("bind %d: %v", i, err)
		}
		defer c.Close()
		conns[i] = c
	}
	x, y, z := conns[0], conns[1], conns[2]

	// Y、Z 收到 X 的探测后原路回一个探测（模拟真实对端的打洞应答）。
	echo := func(c *net.UDPConn) {
		go func() {
			buf := make([]byte, 64)
			for {
				_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
				_, from, err := c.ReadFromUDP(buf)
				if err != nil {
					return
				}
				_, _ = c.WriteToUDP(probeMagic, from)
			}
		}()
	}
	echo(y)
	echo(z)

	cands := []*net.UDPAddr{
		{IP: net.IPv4(127, 0, 0, 1), Port: y.LocalAddr().(*net.UDPAddr).Port},
		{IP: net.IPv4(127, 0, 0, 1), Port: z.LocalAddr().(*net.UDPAddr).Port},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	tun, err := punchConnWindow(ctx, x, cands, 300*time.Millisecond)
	if err != nil {
		t.Fatalf("punch: %v", err)
	}
	defer tun.Close()

	peers := tun.Peers()
	if len(peers) < 2 {
		t.Fatalf("want >=2 collected peers, got %d: %v", len(peers), peers)
	}
	known := map[string]bool{}
	for _, a := range peers {
		known[a.String()] = true
	}
	for _, want := range []string{cands[0].String(), cands[1].String()} {
		if !known[want] {
			t.Fatalf("collected peers %v missing %s", peers, want)
		}
	}
}

// 数据帧按目的 MAC 路由到对应对端；未注册 MAC 返回 ErrNoRoute 且不影响隧道与其他对端。
func TestWriteRoutesByDestinationMAC(t *testing.T) {
	tunA, tunB, macA, macB := punchPair(t)
	defer tunA.Close()
	defer tunB.Close()

	// 未注册的目的 MAC：ErrNoRoute，且隧道不被回退（Peer/knowAddr 仍可用）。
	unknown := net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0xEE}
	frame := ethernet.BuildFrame(unknown, macA, ethernet.TypeIPv4, []byte("to-unknown"))
	if err := tunA.Write(frame); !errors.Is(err, ErrNoRoute) {
		t.Fatalf("want ErrNoRoute, got %v", err)
	}
	// 隧道仍然存活：B 可正常收到 A 发给 B 的帧。
	payload := ethernet.BuildFrame(macB, macA, ethernet.TypeIPv4, []byte("hello-mac-route"))
	if err := tunA.Write(payload); err != nil {
		t.Fatalf("A write: %v", err)
	}
	tunB.SetReadDeadline(time.Now().Add(1 * time.Second))
	got, err := tunB.Read()
	if err != nil {
		t.Fatalf("B read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("B read %v, want %v", got, payload)
	}
}

// 单侧打洞失败（对端无监听）→ 超时回退。
func TestPunchTimeoutFallsBack(t *testing.T) {
	deadPort := freeUDPPort(t)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	tun, err := Punch(ctx, 0, []*net.UDPAddr{mustUDPAddr(t, "127.0.0.1", deadPort)})
	if err != ErrPunchTimeout {
		t.Fatalf("want ErrPunchTimeout, got %v", err)
	}
	if tun != nil {
		t.Fatal("tunnel should be nil on timeout")
	}
}

// 打洞成功后空闲超时回退。
func TestTunnelIdleTimeout(t *testing.T) {
	tunA, tunB, _, _ := punchPair(t)
	defer tunA.Close()
	defer tunB.Close()

	stop := tunA.AttachIdleMonitor(150*time.Millisecond, log.New(io.Discard, "", 0))
	defer stop()
	select {
	case <-tunA.Done():
	case <-time.After(1 * time.Second):
		t.Fatal("idle monitor did not close tunnel")
	}
}

// 多对同时打洞互不干扰。
func TestPunchConcurrentPairs(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, b, _, _ := punchPair(t)
			a.Close()
			b.Close()
		}()
	}
	wg.Wait()
}

// 单客户端：打洞对象不存在（空候选）→ 超时回退，不 panic。
func TestPunchNoCandidates(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if tun, err := Punch(ctx, 0, nil); err != ErrPunchTimeout || tun != nil {
		t.Fatalf("want ErrPunchTimeout+nil, got %v %v", tun, err)
	}
}

// 建两两打洞的隧道对，供子测试复用。
// 显式绑定两个 UDP 连接（取实际端口）再打洞，避免并发场景下 freeUDPPort 的端口复用竞态。
// 返回 (tunA, tunB, macA, macB) 并预注册双向帧路由：p2p 层测试只关心
// UDP 路径，MAC 用约定的测试值；client 层的"候选回查注册"在 client 自己的测试里覆盖。
func punchPair(t *testing.T) (*Tunnel, *Tunnel, net.HardwareAddr, net.HardwareAddr) {
	t.Helper()
	connA, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		t.Fatalf("bind A: %v", err)
	}
	connB, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		connA.Close()
		t.Fatalf("bind B: %v", err)
	}
	aport := connA.LocalAddr().(*net.UDPAddr).Port
	bport := connB.LocalAddr().(*net.UDPAddr).Port

	ctxA, cancelA := context.WithTimeout(context.Background(), 3*time.Second)
	ctxB, cancelB := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelA()
	defer cancelB()

	type res struct {
		tun *Tunnel
		err error
	}
	results := make(chan res, 2)
	go func() {
		tun, err := punchConnWindow(ctxA, connA, []*net.UDPAddr{mustUDPAddr(t, "127.0.0.1", bport)}, 100*time.Millisecond)
		results <- res{tun, err}
	}()
	go func() {
		tun, err := punchConnWindow(ctxB, connB, []*net.UDPAddr{mustUDPAddr(t, "127.0.0.1", aport)}, 100*time.Millisecond)
		results <- res{tun, err}
	}()
	ra := <-results
	rb := <-results
	if ra.err != nil {
		t.Fatalf("A punch: %v", ra.err)
	}
	if rb.err != nil {
		t.Fatalf("B punch: %v", rb.err)
	}
	macA := net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0xaa, 0x01}
	macB := net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0xaa, 0x02}
	ra.tun.RegisterRoute(macB, ra.tun.Peer()) // A -> B：A 学到的 B 地址
	rb.tun.RegisterRoute(macA, rb.tun.Peer()) // B -> A：B 学到的 A 地址
	return ra.tun, rb.tun, macA, macB
}

// Discover 对真实 STUN 服务器探测，应返回非空公网地址。
func TestDiscoverAgainstStunServer(t *testing.T) {
	srv := stunserver.New(stunserver.Config{ListenAddr: "127.0.0.1:0"})
	go srv.Start()
	defer srv.Stop()
	var saddr string
	for i := 0; i < 200; i++ {
		if a := srv.Addr(); a != nil {
			saddr = a.String()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if saddr == "" {
		t.Fatal("stun server did not start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, got, err := Discover(ctx, saddr, 0)
	if conn != nil {
		defer conn.Close()
	}
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got.IP == nil || got.Port == 0 {
		t.Fatalf("bad discovered addr: %v", got)
	}
	// 服务器在 127.0.0.1 上观察到的来源应回环到 127.0.0.1。
	if got.IP.String() != "127.0.0.1" {
		t.Fatalf("discovered ip = %v, want 127.0.0.1", got.IP)
	}
}

// Discover 指向无人端口（读超时）→ 返回错误。
func TestDiscoverContextCancel(t *testing.T) {
	deadPort := freeUDPPort(t)
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	if _, _, err := Discover(ctx, net.JoinHostPort("127.0.0.1", itoa(deadPort)), 0); err == nil {
		t.Fatal("want error for unreachable stun")
	}
}

// 编码往返：decodeXORMapped 应还原 encodeXOR 的输出。
func TestDecodeXORMappedRoundtrip(t *testing.T) {
	ip := net.ParseIP("203.0.113.9").To4()
	port := 34567
	key := []byte{0x21, 0x12, 0xA4, 0x42}
	raw := make([]byte, 8)
	raw[0] = 1
	binary.BigEndian.PutUint16(raw[1:3], uint16(port)^binary.BigEndian.Uint16(key[0:2]))
	for i := 0; i < 4; i++ {
		raw[4+i] = ip[i] ^ key[i%4]
	}
	addr, err := decodeXORMapped(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if addr.IP.String() != "203.0.113.9" || addr.Port != port {
		t.Fatalf("got %v, want 203.0.113.9:%d", addr, port)
	}
	// 非法长度/家族应报错。
	if _, err := decodeXORMapped([]byte{0, 1, 2}); err == nil {
		t.Fatal("short raw should error")
	}
	if _, err := decodeXORMapped(append([]byte{2}, make([]byte, 7)...)); err == nil {
		t.Fatal("non-ipv4 family should error")
	}
}

func freeUDPPort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	p := c.LocalAddr().(*net.UDPAddr).Port
	c.Close()
	return p
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// 对端换地址（CGNAT 重映射）且从未注册候选时，收到的数据帧应自动教会
// 本端该源 MAC 的新地址，使回程帧直达新地址。
func TestTunnelLearnsRouteFromDataFrame(t *testing.T) {
	tunA, tunB, macA, macB := punchPair(t)
	defer tunA.Close()
	defer tunB.Close()
	_ = macA

	// C 扮演"地址漂移后的对端"：新 UDP socket，无候选、无打洞。
	connC, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("bind C: %v", err)
	}
	defer connC.Close()
	addrC := connC.LocalAddr().(*net.UDPAddr)
	macC := net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0xcc, 0x01}

	// C 向 B 直接发数据帧。
	frame := ethernet.BuildFrame(macB, macC, ethernet.TypeIPv4, []byte("roaming-peer"))
	// B 的隧道 socket 可能是双栈 :: 监听，统一按 v4 回环寻址。
	destB := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: tunB.Addr().(*net.UDPAddr).Port}
	if _, err := connC.WriteToUDP(frame, destB); err != nil {
		t.Fatalf("C write: %v", err)
	}

	// B 读到帧，且学到 macC -> addrC。
	_ = tunB.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, err := tunB.Read()
	if err != nil {
		t.Fatalf("B read: %v", err)
	}
	if !bytes.Equal(got, frame) {
		t.Fatalf("B 收到 %v want %v", got, frame)
	}
	if route := tunB.Routes()[macC.String()]; route != addrC.String() {
		t.Fatalf("B 未学习 macC 路由: got=%v want=%s", route, addrC)
	}

	// B 向 macC 回帧应直达 C（不经中继、不丢）。
	reply := ethernet.BuildFrame(macC, macB, ethernet.TypeIPv4, []byte("reply"))
	if err := tunB.Write(reply); err != nil {
		t.Fatalf("B write: %v", err)
	}
	_ = connC.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, maxUDPFrame)
	n, _, err := connC.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("C read: %v", err)
	}
	if !bytes.Equal(buf[:n], reply) {
		t.Fatalf("C 收到 %v want %v", buf[:n], reply)
	}
}

// 路由按地址驱逐：同一地址只允许一个 MAC 键。
// 回归：服务器按会话顺序重分 MAC，重连 peer 继承旧 peer 的 MAC，
// 而本地 routes 跨隧道存活，旧 (MAC->addr) 残留会错投帧。
func TestRegisterRouteEvictsStaleMACForSameAddr(t *testing.T) {
	tunA, tunB, _, _ := punchPair(t)
	defer tunA.Close()
	defer tunB.Close()

	addrX := tunB.Peer() // 任意一个真实可达地址
	macOld := net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0xEE}
	macNew := net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0xEF}

	tunA.RegisterRoute(macOld, addrX)
	if got := tunA.Routes()[macOld.String()]; got != addrX.String() {
		t.Fatalf("预置 stale 路由失败: got=%v", got)
	}
	// 同一地址换新 MAC 注册：旧键应被驱逐。
	tunA.RegisterRoute(macNew, addrX)
	routes := tunA.Routes()
	if _, ok := routes[macOld.String()]; ok {
		t.Fatalf("旧 MAC 键未被驱逐: %v", routes)
	}
	if got := routes[macNew.String()]; got != addrX.String() {
		t.Fatalf("新路由缺失: got=%v", got)
	}
}

// 数据帧学习同样按地址驱逐：漂移对端用同一地址新 MAC 发帧时，
// 指向该地址的旧 MAC 键必须被清掉。
func TestLearnRouteEvictsStaleMACForSameAddr(t *testing.T) {
	tunA, tunB, macA, macB := punchPair(t)
	defer tunA.Close()
	defer tunB.Close()
	_ = macA

	// C：原始 socket，地址固定 addrC。
	connC, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("bind C: %v", err)
	}
	defer connC.Close()
	addrC := connC.LocalAddr().(*net.UDPAddr)
	destB := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: tunB.Addr().(*net.UDPAddr).Port}

	// 预置：B 的路由表里 macOld -> addrC（模拟旧的残留条目）。
	macOld := net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0xEE}
	macNew := net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0xEF}
	tunB.RegisterRoute(macOld, addrC)

	// C 以 macNew 为源 MAC 发数据帧给 B。
	frame := ethernet.BuildFrame(macB, macNew, ethernet.TypeIPv4, []byte("evict-me"))
	if _, err := connC.WriteToUDP(frame, destB); err != nil {
		t.Fatalf("C write: %v", err)
	}
	_ = tunB.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := tunB.Read(); err != nil {
		t.Fatalf("B read: %v", err)
	}

	routes := tunB.Routes()
	if _, ok := routes[macOld.String()]; ok {
		t.Fatalf("学习后旧 MAC 键未被驱逐: %v", routes)
	}
	if got := routes[macNew.String()]; got != addrC.String() {
		t.Fatalf("未学到新路由: got=%v want=%s", got, addrC)
	}
}

// TunnelProbeC: 构造一个带 Read 循环的隧道，用于 ProbeRoute 测试中对端自动回探测。
func newBareTunnel(t *testing.T) *Tunnel {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	tun := &Tunnel{
		conn:       conn,
		done:       make(chan struct{}),
		routes:     make(map[string]*net.UDPAddr),
		addrs:      make(map[string]*net.UDPAddr, 8),
		probeReply: make(map[string]time.Time, 8),
	}
	return tun
}

// readLoop 持续读取并吞掉数据帧（触发 addPeer/探测回复），隧道关闭后退出。
func readLoop(t *testing.T, tun *Tunnel) {
	t.Helper()
	go func() {
		for {
			if _, err := tun.Read(); err != nil {
				return
			}
		}
	}()
}

// 隧道存活但对端从未在打洞窗口内回过包：主动探测应拿到直连路由。
// 回归缺陷 B：旧实现仅靠 KnownAddr 动态注册，后上线/重连的 peer 永远无路由。
func TestProbeRouteRegistersMissingPeer(t *testing.T) {
	tunA, _, macA, _ := punchPair(t)
	defer tunA.Close()
	macC := net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0xcc, 0x02}
	_ = macA

	tunC := newBareTunnel(t)
	defer tunC.Close()
	addrC := tunC.conn.LocalAddr().(*net.UDPAddr)

	// 双方都要有 Read 循环：A 的循环把 C 的回包记入 addrs（KnownAddr），
	// C 的循环收到探测后按限速原路回复，同时捕获数据帧用于断言。
	frames := make(chan []byte, 8)
	go func() {
		for {
			buf, err := tunC.Read()
			if err != nil {
				return
			}
			select {
			case frames <- buf:
			default:
			}
		}
	}()
	readLoop(t, tunA)

	if tunA.HasRoute(macC) {
		t.Fatal("预置条件不成立：A 已有 macC 路由")
	}
	if !tunA.ProbeRoute(macC, []*net.UDPAddr{addrC}, 3*time.Second) {
		t.Fatal("ProbeRoute 未能在超时内注册路由")
	}
	if got := tunA.Routes()[macC.String()]; got != addrC.String() {
		t.Fatalf("注册的地址错误: got=%v want=%s", got, addrC)
	}

	// 数据面验证：A 向 macC 发帧应直达 C。
	frame := ethernet.BuildFrame(macC, macA, ethernet.TypeIPv4, []byte("direct-after-probe"))
	if err := tunA.Write(frame); err != nil {
		t.Fatalf("A write: %v", err)
	}
	select {
	case got := <-frames:
		if !bytes.Equal(got, frame) {
			t.Fatalf("C 收到 %v want %v", got, frame)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("C 未收到 A 的直连帧")
	}
}

// 探测无应答时按时返回 false，不卡死。
func TestProbeRouteTimeoutNoReply(t *testing.T) {
	tunA, _, _, _ := punchPair(t)
	defer tunA.Close()
	readLoop(t, tunA)

	// 无人监听的端口。
	dead := mustUDPAddr(t, "127.0.0.1", 1)
	macC := net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0xcc, 0x03}
	start := time.Now()
	if tunA.ProbeRoute(macC, []*net.UDPAddr{dead}, 400*time.Millisecond) {
		t.Fatal("无应答的探测不应成功")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("超时耗时过长: %v", d)
	}
	if tunA.HasRoute(macC) {
		t.Fatal("失败时不应注册路由")
	}
}

// 数据帧收发计数（探测/保活不计入）。
func TestTunnelFrameCounters(t *testing.T) {
	tunA, tunB, macA, macB := punchPair(t)
	defer tunA.Close()
	defer tunB.Close()

	send := func() []byte {
		f := ethernet.BuildFrame(macB, macA, ethernet.TypeIPv4, []byte("count-me"))
		if err := tunA.Write(f); err != nil {
			t.Fatalf("A write: %v", err)
		}
		return f
	}
	f1 := send()
	_ = tunB.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := tunB.Read(); err != nil {
		t.Fatalf("B read: %v", err)
	}

	if n := tunA.TxFrames(); n != 1 {
		t.Fatalf("TxFrames=%d want 1", n)
	}
	if n := tunB.RxFrames(); n != 1 {
		t.Fatalf("RxFrames=%d want 1", n)
	}

	send() // 第二帧不读，仅看发送计数
	if n := tunA.TxFrames(); n != 2 {
		t.Fatalf("TxFrames=%d want 2", n)
	}
	if n := tunB.RxFrames(); n != 1 {
		t.Fatalf("RxFrames=%d want 1（未读第二帧）", n)
	}
	_ = f1
}
