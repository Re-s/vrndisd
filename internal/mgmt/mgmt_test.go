package mgmt

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"testing"
	"time"

	"github.com/Re-s/vrndisd/internal/proto"
)

func quietProvider() proto.ManagementState {
	return proto.ManagementState{Role: "test", Version: "t"}
}

func dialQuery(t *testing.T, addr string) proto.ManagementState {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("连接管理端口 %s 失败: %v", addr, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if err := json.NewEncoder(conn).Encode(proto.ManagementRequest{Cmd: "status"}); err != nil {
		t.Fatalf("发送请求失败: %v", err)
	}
	var st proto.ManagementState
	if err := json.NewDecoder(conn).Decode(&st); err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	return st
}

func waitAddr(t *testing.T, m *Server) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if a := m.Addr(); a != "" {
			return a
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("管理端口未就绪")
	return ""
}

// TestServeQuery - 启动后可查询，provider 状态原样返回。
func TestServeQuery(t *testing.T) {
	m := Serve("", func() proto.ManagementState {
		return proto.ManagementState{Role: "x", Version: "1.2.3"}
	}, log.New(testWriter{t}, "", 0))
	defer m.Close()
	addr := waitAddr(t, m)
	if st := dialQuery(t, addr); st.Version != "1.2.3" || st.Role != "x" {
		t.Fatalf("状态不符: %+v", st)
	}
}

// TestServeDisabled - "-" 禁用：不监听任何端口，Addr 恒空。
func TestServeDisabled(t *testing.T) {
	m := Serve("-", quietProvider, log.New(testWriter{t}, "", 0))
	defer m.Close()
	if a := m.Addr(); a != "" {
		t.Fatalf("禁用时不应有地址: %s", a)
	}
}

// TestPortFallback - 默认端口被占时自动避让到下一端口，Addr 反映实际端口。
func TestPortFallback(t *testing.T) {
	// 预占 7790
	occ, err := net.Listen("tcp", "127.0.0.1:7790")
	if err != nil {
		t.Skipf("无法预占 7790: %v", err)
	}
	defer occ.Close()

	m := Serve("", quietProvider, log.New(testWriter{t}, "", 0))
	defer m.Close()
	addr := waitAddr(t, m)
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "7790" {
		t.Fatalf("应避让到非 7790 端口，实际 %s", addr)
	}
	dialQuery(t, addr)
}

// TestExplicitNoFallback - 显式端口被占时不避让，按 watchInterval 退避重试，
// 释放后恢复监听（与旧版服务器行为一致）。
func TestExplicitNoFallback(t *testing.T) {
	SetWatchIntervalForTest(50 * time.Millisecond)
	t.Cleanup(func() { SetWatchIntervalForTest(10 * time.Second) })

	occ, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := occ.Addr().String()

	m := Serve(addr, quietProvider, log.New(testWriter{t}, "", 0))
	defer m.Close()
	time.Sleep(150 * time.Millisecond) // 确保已尝试并失败
	if a := m.Addr(); a != "" {
		t.Fatalf("端口被占时不应监听成功: %s", a)
	}

	occ.Close() // 释放，触发重试
	got := waitAddr(t, m)
	if got != addr {
		t.Fatalf("显式端口应重试原地址，期望 %s 得 %s", addr, got)
	}
	dialQuery(t, got)
}

// TestAllPortsBusy - 默认区间全部被占时退避重试，释放其一后恢复。
func TestAllPortsBusy(t *testing.T) {
	SetWatchIntervalForTest(50 * time.Millisecond)
	t.Cleanup(func() { SetWatchIntervalForTest(10 * time.Second) })

	var occ []net.Listener
	defer func() {
		for _, ln := range occ {
			ln.Close()
		}
	}()
	for i := 0; i < portTries; i++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", 7790+i))
		if err != nil {
			t.Skipf("无法预占端口: %v", err)
		}
		occ = append(occ, ln)
	}

	m := Serve("", quietProvider, log.New(testWriter{t}, "", 0))
	defer m.Close()
	time.Sleep(150 * time.Millisecond)
	if a := m.Addr(); a != "" {
		t.Fatalf("全部端口被占时不应监听成功: %s", a)
	}

	occ[0].Close() // 释放 7790
	got := waitAddr(t, m)
	if _, port, _ := net.SplitHostPort(got); port != "7790" {
		t.Fatalf("释放后应回落到 7790，实际 %s", got)
	}
	dialQuery(t, got)
}

// TestCloseStopsPort - Close 后端口立即不可再连，Addr 返回空。
func TestCloseStopsPort(t *testing.T) {
	m := Serve("", quietProvider, log.New(testWriter{t}, "", 0))
	addr := waitAddr(t, m)
	dialQuery(t, addr)
	m.Close()

	if a := m.Addr(); a != "" {
		t.Fatalf("Close 后 Addr 应为空: %s", a)
	}
	if conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond); err == nil {
		conn.Close()
		t.Fatalf("Close 后端口应已关闭: %s", addr)
	}
}

// testWriter 把管理面日志导出到测试输出（出问题时可见）。
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)
	return len(p), nil
}
