package p2p

import (
	"context"
	"encoding/binary"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	stunserver "vrndis/internal/stun/server"
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
	tunA, tunB := punchPair(t)
	defer tunA.Close()
	defer tunB.Close()

	if tunA.Peer() == nil || tunB.Peer() == nil {
		t.Fatal("tunnel has no peer")
	}

	// 双向传帧。
	if err := tunA.Write([]byte("ping-from-A")); err != nil {
		t.Fatalf("A write: %v", err)
	}
	tunB.SetReadDeadline(time.Now().Add(1 * time.Second))
	gotB, err := tunB.Read()
	if err != nil || string(gotB) != "ping-from-A" {
		t.Fatalf("B read: %v %q", err, gotB)
	}
	if err := tunB.Write([]byte("pong-from-B")); err != nil {
		t.Fatalf("B write: %v", err)
	}
	tunA.SetReadDeadline(time.Now().Add(1 * time.Second))
	gotA, err := tunA.Read()
	if err != nil || string(gotA) != "pong-from-B" {
		t.Fatalf("A read: %v %q", err, gotA)
	}
}

// 保活探测帧必须被数据面 Read 吸收，不污染 L2 数据流。
// 回归：此前 keepalive 探测与数据共用同一 socket，读到的真实数据帧之前可能先读到对端保活探测。
func TestReadAbsorbsKeepaliveProbe(t *testing.T) {
	tunA, tunB := punchPair(t)
	defer tunA.Close()
	defer tunB.Close()

	// 手动向 A 注入一个保活探测帧（等价于 B 的 keepalive 发出，但直接从 B 发）
	// 以确定 A 的 Read 会跳过它并返回随后到来的真实数据。
	if err := tunB.Write(probeMagic); err != nil {
		t.Fatalf("B write probe: %v", err)
	}
	// 确保探测帧先于数据帧到达 A。
	time.Sleep(50 * time.Millisecond)
	if err := tunB.Write([]byte("real-data-after-probe")); err != nil {
		t.Fatalf("B write data: %v", err)
	}

	tunA.SetReadDeadline(time.Now().Add(1 * time.Second))
	got, err := tunA.Read()
	if err != nil {
		t.Fatalf("A read: %v", err)
	}
	if string(got) != "real-data-after-probe" {
		t.Fatalf("A read got %q, want data (probe must be absorbed)", got)
	}
}

// 回归：P2P 探测包乒乓风暴。
// 旧实现 Tunnel.Read 收到探测帧会无条件原路回一个探测帧，双方互答回包
// 形成无速率限制的自激回路（A 回 B -> B 收到又回 A -> ...），实测双向
// ~27k pps、6µs 背靠背包间隔、99.8% 空转流量。本测试模拟"狂暴对端"
// （如旧版本节点或异常/恶意来源）向 A 背靠背狂发 400 个探测帧，断言 A
// 的回复被按来源限速压到个位数，而不是每包必回。
func TestProbeReplyRateLimited(t *testing.T) {
	tunA, tunB := punchPair(t)
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
	tunA, tunB := punchPair(t)
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
				_ = tunA.Write([]byte("payload"))
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
	tunA, tunB := punchPair(t)
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
			a, b := punchPair(t)
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
func punchPair(t *testing.T) (*Tunnel, *Tunnel) {
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
		tun, err := punchConn(ctxA, connA, []*net.UDPAddr{mustUDPAddr(t, "127.0.0.1", bport)})
		results <- res{tun, err}
	}()
	go func() {
		tun, err := punchConn(ctxB, connB, []*net.UDPAddr{mustUDPAddr(t, "127.0.0.1", aport)})
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
	return ra.tun, rb.tun
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
