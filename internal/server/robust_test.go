// 健壮性测试：异常输入、连接风暴、资源回收、慢消费者、并发负载、崩溃恢复。
// 详细测试规划见 docs/robustness-plan.md。
package server_test

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"vrndis/internal/client"
	"vrndis/internal/ethernet"
	"vrndis/internal/proto"
	"vrndis/internal/server"
	"vrndis/internal/tap"
)

// ---- 1. 连接风暴：频繁上下线不得泄漏资源 ----

func TestConnectionChurnNoLeak(t *testing.T) {
	s := newStack(t)
	for i := 0; i < 100; i++ {
		c, _, _ := s.connect(fmt.Sprintf("churn%d", i))
		c.Stop()
	}
	// 风暴后地址池必须完整回收（无泄漏），会话数归零
	waitUntil(t, 3*time.Second, func() bool {
		return s.srv.SessionCount() == 0 && s.srv.FreeAddresses() == 253
	})
	_, _, a := s.connect("after1")
	_, _, b := s.connect("after2")
	if a.IP == b.IP {
		t.Fatalf("风暴后地址重复: %s", a.IP)
	}
}

func TestReconnectSameSlotStable(t *testing.T) {
	s := newStack(t)
	for i := 0; i < 30; i++ {
		_, _, a := s.connect(fmt.Sprintf("re%d", i))
		if a.IP != "10.77.0.2" {
			t.Fatalf("第 %d 次重连地址 %s，期望 10.77.0.2", i, a.IP)
		}
		s.clients[len(s.clients)-1].Stop()
		waitUntil(t, 2*time.Second, func() bool { return s.srv.SessionCount() == 0 })
	}
}

// ---- 2. 地址池耗尽与恢复 ----

func TestPoolExhaustionAndRecovery(t *testing.T) {
	srvDev := tap.NewFake("srv")
	srv, err := server.New(server.Config{
		Listen: "127.0.0.1:0", Network: "10.77.0.0/30", // 仅 1 个可用地址 .2
		Gateway: "10.77.0.1", TapMAC: "02:00:00:00:00:00", Device: srvDev,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	addr := fmt.Sprintf("127.0.0.1:%d", srv.Port())
	c1 := client.New(client.Config{Server: addr, Name: "first", Device: tap.NewFake("c1")})
	a1, err := c1.Connect()
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Stop()
	if a1.IP != "10.77.0.2" {
		t.Fatalf("first: %s", a1.IP)
	}
	// 第二个客户端必须被拒绝（服务器在分配失败时关闭连接），且服务器不崩溃
	c2 := client.New(client.Config{Server: addr, Name: "second", Device: tap.NewFake("c2")})
	if _, err := c2.Connect(); err == nil {
		c2.Stop()
		t.Fatal("地址池耗尽时新客户端应连接失败")
	}
	// 释放后恢复
	c1.Stop()
	waitUntil(t, 2*time.Second, func() bool { return srv.SessionCount() == 0 })
	c3 := client.New(client.Config{Server: addr, Name: "third", Device: tap.NewFake("c3")})
	defer c3.Stop()
	a3, err := c3.Connect()
	if err != nil {
		t.Fatal(err)
	}
	if a3.IP != "10.77.0.2" {
		t.Fatalf("recovery: %s", a3.IP)
	}
}

// ---- 3. 畸形帧 / 垃圾输入不得影响会话与服务器 ----

func TestMalformedFramesDoNotKillSession(t *testing.T) {
	s := newStack(t)
	_, dev, a := s.connect("cli")
	bad := [][]byte{
		{},
		{0x01, 0x02, 0x03},
		make([]byte, 13),
		{0xde, 0xad, 0xbe, 0xef},
	}
	for _, b := range bad {
		_ = dev.Inject(b)
	}
	frame := ethernet.BuildFrame(
		mustMAC(t, a.ServerMAC), mustMAC(t, a.MAC), ethernet.TypeIPv4, []byte("still-alive"))
	if err := dev.Inject(frame); err != nil {
		t.Fatal(err)
	}
	got := recvFrame(t, s.srvDev)
	if string(got) != string(frame) {
		t.Fatal("畸形帧之后合法帧未通过")
	}
}

func TestGarbageHelloTolerated(t *testing.T) {
	s := newStack(t)
	conn := rawDial(t, s.srv.Port())
	defer conn.Close()
	readAssign(t, conn)
	if err := proto.WriteMessage(conn, proto.TypeHello, []byte("not-json{{{")); err != nil {
		t.Fatal(err)
	}
	if err := proto.WriteMessage(conn, 200, []byte("{}")); err != nil { // 未知类型
		t.Fatal(err)
	}
	frame := ethernet.BuildFrame(
		mustMAC(t, "02:00:00:00:00:00"), mustMAC(t, "02:00:00:00:00:01"),
		ethernet.TypeIPv4, []byte("after-garbage"))
	if err := proto.WriteMessage(conn, proto.TypeData, frame); err != nil {
		t.Fatal(err)
	}
	got := recvFrame(t, s.srvDev)
	if string(got) != string(frame) {
		t.Fatal("垃圾控制消息后数据面应正常")
	}
}

func TestOversizedMessageKillsOnlyThatSession(t *testing.T) {
	s := newStack(t)
	conn := rawDial(t, s.srv.Port())
	readAssign(t, conn)

	hdr := make([]byte, 5) // 伪造超长 header
	binary.BigEndian.PutUint32(hdr, proto.MaxBody+1)
	hdr[4] = proto.TypeData
	if _, err := conn.Write(hdr); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Read(make([]byte, 16)); err == nil {
		t.Fatal("服务器应关闭违规连接")
	}
	conn.Close()
	waitUntil(t, 2*time.Second, func() bool { return s.srv.SessionCount() == 0 })

	_, _, a := s.connect("healthy")
	if a.IP != "10.77.0.2" {
		t.Fatalf("服务器未恢复: %s", a.IP)
	}
}

// ---- 4. 慢消费者不得拖死交换机 ----

func TestSlowConsumerDoesNotBlockOthers(t *testing.T) {
	s := newStack(t)
	// 模拟内核持续消费服务器网卡（真实内核总是消费的）
	go func() {
		for {
			if _, err := s.srvDev.Capture(); err != nil {
				return
			}
		}
	}()

	s.connect("slow") // 慢消费者：永不读取自己的网卡
	_, devB, aB := s.connect("b")
	_, devC, aC := s.connect("c")

	// C 侧持续排空并寻找目标单播帧
	uni := ethernet.BuildFrame(
		mustMAC(t, aC.MAC), mustMAC(t, aB.MAC), ethernet.TypeIPv4, []byte("priority"))
	found := make(chan struct{})
	go func() {
		for {
			f, err := devC.Capture()
			if err != nil {
				return
			}
			if string(f) == string(uni) {
				close(found)
				return
			}
		}
	}()

	var floodWG sync.WaitGroup
	floodWG.Add(1)
	go func() {
		defer floodWG.Done()
		for i := 0; i < 2000; i++ {
			frame := ethernet.BuildFrame(
				ethernet.Broadcast, mustMAC(t, aB.MAC), ethernet.TypeIPv4,
				[]byte(fmt.Sprintf("flood-%d", i)))
			if err := devB.Inject(frame); err != nil {
				return
			}
		}
	}()

	// 泛洪进行中发送单播，必须在期限内到达
	time.Sleep(20 * time.Millisecond)
	if err := devB.Inject(uni); err != nil {
		t.Fatal(err)
	}
	select {
	case <-found:
	case <-time.After(10 * time.Second):
		t.Fatal("慢消费者导致单播帧饿死")
	}

	// 泛洪进行中，新客户端仍能即时接入
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, _, _ := s.connect("late")
		_ = c
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("慢消费者阻塞了新客户端接入")
	}
	floodWG.Wait()
}

// ---- 5. 并发负载 ----

// TestConcurrentUnicastLoadPaced 稳态节奏流量必须 100% 送达（零丢帧）。
func TestConcurrentUnicastLoadPaced(t *testing.T) {
	s := newStack(t)
	_, hubDev, hubA := s.connect("hub")

	var received atomic.Int64
	go func() {
		for {
			f, err := hubDev.Capture()
			if err != nil {
				return
			}
			if len(f) > 0 {
				received.Add(1)
			}
		}
	}()

	// hub 先发一帧，让交换机学习其 MAC（后续为真单播，不放大）
	_ = hubDev.Inject(ethernet.BuildFrame(
		ethernet.Broadcast, mustMAC(t, hubA.MAC), ethernet.TypeARP, []byte("learn")))
	time.Sleep(50 * time.Millisecond)

	const senders, perSender = 9, 50
	var wg sync.WaitGroup
	for i := 0; i < senders; i++ {
		_, dev, a := s.connect(fmt.Sprintf("snd%d", i))
		drainInBackground(dev) // 发送者网卡也要排空（泛洪副本不致背压）
		wg.Add(1)
		go func(dev *tap.Fake, mac string) {
			defer wg.Done()
			for j := 0; j < perSender; j++ {
				frame := ethernet.BuildFrame(
					mustMAC(t, hubA.MAC), mustMAC(t, mac), ethernet.TypeIPv4,
					[]byte(fmt.Sprintf("load-%d", j)))
				if err := dev.Inject(frame); err != nil {
					return
				}
				time.Sleep(2 * time.Millisecond) // 稳态节奏：每发送者 ~500fps
			}
		}(dev, a.MAC)
	}
	wg.Wait()

	total := senders * perSender
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && received.Load() < int64(total) {
		time.Sleep(20 * time.Millisecond)
	}
	got := received.Load()
	t.Logf("稳态负载: 发送 %d 帧 / 收到 %d 帧", total, got)
	if got != int64(total) {
		t.Fatalf("稳态流量应零丢帧: %d/%d", got, total)
	}
}

// TestBurstNeverDeadlocks 极端微爆发（8ms 内 900 帧）：允许背压丢帧，
// 但绝不允许死锁、饿死或服务不可用（交换机过载丢帧是协议语义的一部分）。
func TestBurstNeverDeadlocks(t *testing.T) {
	s := newStack(t)
	_, hubDev, hubA := s.connect("hub")

	var received atomic.Int64
	go func() {
		for {
			f, err := hubDev.Capture()
			if err != nil {
				return
			}
			if len(f) > 0 {
				received.Add(1)
			}
		}
	}()

	// hub 先发一帧，让交换机学习其 MAC
	_ = hubDev.Inject(ethernet.BuildFrame(
		ethernet.Broadcast, mustMAC(t, hubA.MAC), ethernet.TypeARP, []byte("learn")))
	time.Sleep(50 * time.Millisecond)

	const senders, perSender = 9, 100
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < senders; i++ {
		_, dev, a := s.connect(fmt.Sprintf("snd%d", i))
		drainInBackground(dev)
		wg.Add(1)
		go func(dev *tap.Fake, mac string) {
			defer wg.Done()
			for j := 0; j < perSender; j++ {
				frame := ethernet.BuildFrame(
					mustMAC(t, hubA.MAC), mustMAC(t, mac), ethernet.TypeIPv4,
					[]byte(fmt.Sprintf("load-%d", j)))
				if err := dev.Inject(frame); err != nil {
					return
				}
			}
		}(dev, a.MAC)
	}
	wg.Wait()
	elapsed := time.Since(start)

	total := senders * perSender
	time.Sleep(500 * time.Millisecond)
	got := received.Load()
	t.Logf("微爆发: 发送 %d 帧 / 收到 %d 帧 (%.1f%%) / 发送耗时 %v",
		total, got, float64(got)*100/float64(total), elapsed)
	if got < int64(total)/10 {
		t.Fatalf("爆发下送达率过低: %d/%d", got, total)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("发送方被阻塞过久: %v", elapsed)
	}

	// 爆发后服务必须立即可用
	waitUntil(t, 2*time.Second, func() bool { return s.srv.SessionCount() == int(senders)+1 })
	_, _, a := s.connect("post-burst")
	if a.IP == "" {
		t.Fatal("爆发后新客户端应能接入")
	}
}

func TestLargeFramePassthrough(t *testing.T) {
	s := newStack(t)
	_, dev, a := s.connect("cli")
	payload := make([]byte, 8192)
	for i := range payload {
		payload[i] = byte(i)
	}
	frame := ethernet.BuildFrame(
		mustMAC(t, a.ServerMAC), mustMAC(t, a.MAC), ethernet.TypeIPv4, payload)
	if err := dev.Inject(frame); err != nil {
		t.Fatal(err)
	}
	got := recvFrame(t, s.srvDev)
	if len(got) != len(frame) {
		t.Fatalf("大帧长度不一致: %d vs %d", len(got), len(frame))
	}
	for i := range payload {
		if got[14+i] != payload[i] {
			t.Fatalf("大帧内容损坏 @%d", i)
		}
	}
}

// ---- 6. 崩溃恢复与资源回收 ----

func TestServerStopDisconnectsClients(t *testing.T) {
	s := newStack(t)
	var cs []*client.Client
	for i := 0; i < 3; i++ {
		c, _, _ := s.connect(fmt.Sprintf("x%d", i))
		cs = append(cs, c)
	}
	s.srv.Stop()
	for _, c := range cs {
		waitUntil(t, 2*time.Second, c.Closed)
	}
}

func TestGoroutinesReturnToBaseline(t *testing.T) {
	s := newStack(t)
	_, _, _ = s.connect("warm") // 预热
	waitUntil(t, 2*time.Second, func() bool { return s.srv.SessionCount() == 1 })
	s.clients[0].Stop()
	waitUntil(t, 2*time.Second, func() bool { return s.srv.SessionCount() == 0 })
	time.Sleep(200 * time.Millisecond)
	base := runtime.NumGoroutine()

	for i := 0; i < 30; i++ {
		c, _, _ := s.connect(fmt.Sprintf("g%d", i))
		c.Stop()
	}
	waitUntil(t, 3*time.Second, func() bool { return s.srv.SessionCount() == 0 })
	time.Sleep(300 * time.Millisecond)
	now := runtime.NumGoroutine()
	if now > base+15 {
		t.Fatalf("协程疑似泄漏: 基线 %d -> 现在 %d", base, now)
	}
	t.Logf("协程数: 基线 %d -> 现在 %d", base, now)
}

// drainInBackground 持续排空网卡收包（模拟操作系统内核总是消费）。
func drainInBackground(dev *tap.Fake) {
	go func() {
		for {
			if _, err := dev.Capture(); err != nil {
				return
			}
		}
	}()
}

// ---- 裸连接工具（协议层测试用）----

func rawDial(t *testing.T, port int) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

func readAssign(t *testing.T, conn net.Conn) proto.Assignment {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	defer conn.SetReadDeadline(time.Time{})
	msg, err := proto.ReadMessage(conn)
	if err != nil {
		t.Fatalf("读取 ASSIGN 失败: %v", err)
	}
	if msg.Type != proto.TypeAssign {
		t.Fatalf("期望 ASSIGN，收到 %s", proto.TypeName(msg.Type))
	}
	var a proto.Assignment
	if err := json.Unmarshal(msg.Body, &a); err != nil {
		t.Fatal(err)
	}
	return a
}
