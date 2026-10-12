package tap

import (
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

func TestFakeSetGet(t *testing.T) {
	f := NewFake("vrndis-t0")
	if f.Name() != "vrndis-t0" {
		t.Fatalf("name=%q", f.Name())
	}
	mac := net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0x07}
	if err := f.SetMAC(mac); err != nil || f.MAC.String() != mac.String() {
		t.Fatalf("SetMAC: %v %v", err, f.MAC)
	}
	if err := f.SetIPv4(net.ParseIP("10.77.0.9"), 24); err != nil || f.IPv4 != "10.77.0.9/24" {
		t.Fatalf("SetIPv4: %v %q", err, f.IPv4)
	}
	if err := f.Up(); err != nil || !f.UpFlag {
		t.Fatalf("Up: %v", err)
	}
	gw := net.ParseIP("10.77.0.1")
	if err := f.AddDefaultRoute(gw); err != nil || f.Route.String() != gw.String() {
		t.Fatalf("AddDefaultRoute: %v %v", err, f.Route)
	}
}

func TestFakeReadWriteRoundTrip(t *testing.T) {
	f := NewFake("t")
	frame := []byte("frame-from-tunnel")
	if err := f.Write(frame); err != nil {
		t.Fatal(err)
	}
	got, err := f.Capture()
	if err != nil || string(got) != "frame-from-tunnel" {
		t.Fatalf("Capture: %v %q", err, got)
	}
	if err := f.Inject([]byte("frame-to-tunnel")); err != nil {
		t.Fatal(err)
	}
	got, err = f.Read()
	if err != nil || string(got) != "frame-to-tunnel" {
		t.Fatalf("Read: %v %q", err, got)
	}
}

func TestFakeReadBlocksThenReturns(t *testing.T) {
	f := NewFake("t")
	done := make(chan struct{})
	go func() {
		f.Inject([]byte("x"))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not unblock")
	}
	got, _ := f.Read()
	if string(got) != "x" {
		t.Fatalf("got %q", got)
	}
}

func TestFakeCloseUnblocksAndIdempotent(t *testing.T) {
	f := NewFake("t")
	f.Close()
	if _, err := f.Read(); err == nil {
		t.Fatal("Read after close should error")
	}
	if _, err := f.Capture(); err == nil {
		t.Fatal("Capture after close should error")
	}
	if err := f.Write([]byte("x")); err == nil {
		t.Fatal("Write after close should error")
	}
	if err := f.Inject([]byte("x")); err == nil {
		t.Fatal("Inject after close should error")
	}
	if err := f.Close(); err != nil {
		t.Fatalf("double close: %v", err)
	}
}

func TestFakeBufferFullDoesNotBlock(t *testing.T) {
	f := NewFake("t")
	// 写满接收队列
	for i := 0; i < 256; i++ {
		if err := f.Write([]byte{byte(i)}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	// 超容量时应快速返回错误，而非阻塞
	done := make(chan error, 1)
	go func() { done <- f.Write([]byte("overflow")) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("overflow write should error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("overflow write blocked; should return ErrBufferFull")
	}
	// Inject 模拟 OS 向网卡注入：toTunnel 是独立队列，需单独填满。
	for i := 0; i < 256; i++ {
		if err := f.Inject([]byte{byte(i)}); err != nil {
			t.Fatalf("inject %d: %v", i, err)
		}
	}
	// 队列满时 Inject 应背压阻塞（与真实 TAP 语义一致），
	// 消费一帧后自动恢复，而非返回错误或永久卡死。
	done2 := make(chan error, 1)
	go func() { done2 <- f.Inject([]byte("blocked")) }()
	select {
	case err := <-done2:
		t.Fatalf("inject should block on full queue, got %v", err)
	case <-time.After(100 * time.Millisecond):
		// 预期阻塞
	}
	// Inject 写入 toTunnel，故用 Read() 消费一帧解除背压。
	if _, err := f.Read(); err != nil {
		t.Fatalf("read: %v", err)
	}
	select {
	case err := <-done2:
		if err != nil {
			t.Fatalf("inject after drain: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("inject should have resumed after drain")
	}
}

func TestItoaCases(t *testing.T) {
	for n, want := range map[int]string{0: "0", 1: "1", 24: "24", 255: "255", 127: "127"} {
		if got := itoa(n); got != want {
			t.Fatalf("itoa(%d)=%q want %q", n, got, want)
		}
	}
}

func TestErrUnsupportedWrapping(t *testing.T) {
	if !errors.Is(fmt.Errorf("wrap: %w", ErrUnsupported), ErrUnsupported) {
		t.Fatal("wrapped error should match")
	}
	if errors.Is(ErrBufferFull, ErrUnsupported) {
		t.Fatal("distinct errors should not match")
	}
}

// isAlreadyExists 决定某次 `ip ...` 失败是否只是"已存在"的幂等情形而应忽略。
func TestIsAlreadyExists(t *testing.T) {
	cases := []struct {
		name string
		err  error
		out  string
		want bool
	}{
		{"nil error → not exists", nil, "", false},
		{"plain error, empty out", errors.New("boom"), "", false},
		{"RTNETLINK File exists", errors.New("exit status 2"), "RTNETLINK answers: File exists\n", true},
		{"RTNETLINK 其他错误非已存在", errors.New("exit status 2"), "RTNETLINK answers: Network is unreachable\n", false},
		{"File exists 单独命中", errors.New("exit status 2"), "File exists\n", true},
		{"RTNETLINK 操作不允许非已存在", errors.New("exit status 2"), "RTNETLINK answers: Operation not permitted\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isAlreadyExists(c.err, []byte(c.out)); got != c.want {
				t.Fatalf("isAlreadyExists(%v, %q) = %v, want %v", c.err, c.out, got, c.want)
			}
		})
	}
}

// 断言直连子网路由与默认路由都在 AddDefaultRoute 之后生效（Fake 设备语义）。
func TestFakeSubnetRouteAndDefault(t *testing.T) {
	f := NewFake("vrndis-t-sub")
	if err := f.SetIPv4(net.ParseIP("10.77.0.9"), 24); err != nil {
		t.Fatalf("SetIPv4: %v", err)
	}
	// Fake 只记录到默认路由；本测试断言 10.77.0.0/24 与网关 10.77.0.1 同网段，
	// 即网关在直连网段内（内核可做 on-link/ARP 判定的前提）。
	gw := net.ParseIP("10.77.0.1")
	network := net.IPNet{IP: net.IPv4(10, 77, 0, 0), Mask: net.CIDRMask(24, 32)}
	if !network.Contains(gw) {
		t.Fatalf("gateway %v not within subnet %v", gw, network.String())
	}
	if err := f.AddDefaultRoute(gw); err != nil {
		t.Fatalf("AddDefaultRoute: %v", err)
	}
	if f.Route.String() != gw.String() {
		t.Fatalf("Route = %v, want %v", f.Route, gw)
	}
}
