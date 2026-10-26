package server

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/Re-s/vrndisd/internal/mgmt"
	"github.com/Re-s/vrndisd/internal/proto"
	"github.com/Re-s/vrndisd/internal/tap"
)

// mgmtTestServer 启动一个带管理端口（临时回环端口）的测试服务器。
func mgmtTestServer(t *testing.T) *Server {
	t.Helper()
	network, gateway := NewTestNetwork()
	srv, err := New(Config{
		Listen:           "127.0.0.1:0",
		Network:          network,
		Gateway:          gateway,
		TapMAC:           "02:00:00:00:00:00",
		Device:           tap.NewFake("srv"),
		ManagementListen: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Stop() })
	waitManagementReady(t, srv)
	return srv
}

func waitManagementReady(t *testing.T, srv *Server) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if srv.ManagementAddr() != "" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("管理端口未就绪")
}

// mgmtQuery 发一次管理查询并返回状态快照。
func mgmtQuery(t *testing.T, addr string) proto.ManagementState {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("连接管理端口 %s 失败: %v", addr, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if err := json.NewEncoder(conn).Encode(proto.ManagementRequest{Cmd: "clients"}); err != nil {
		t.Fatalf("发送管理请求失败: %v", err)
	}
	var st proto.ManagementState
	if err := json.NewDecoder(conn).Decode(&st); err != nil {
		t.Fatalf("读取管理状态失败: %v", err)
	}
	return st
}

func fastManagementWatch(t *testing.T) {
	t.Helper()
	mgmt.SetWatchIntervalForTest(50 * time.Millisecond)
	t.Cleanup(func() { mgmt.SetWatchIntervalForTest(10 * time.Second) })
}

// TestManagementTCPQuery - 起服后经回环 TCP 管理端口查询在线客户端快照。
func TestManagementTCPQuery(t *testing.T) {
	srv := mgmtTestServer(t)
	st := mgmtQuery(t, srv.ManagementAddr())
	if st.Version == "" {
		t.Fatalf("状态快照缺少版本: %+v", st)
	}
}

// TestManagementDisabled - ManagementListen 为 "-" 时不起管理监听，Stop 也不报错。
func TestManagementDisabled(t *testing.T) {
	network, gateway := NewTestNetwork()
	srv, err := New(Config{
		Listen:           "127.0.0.1:0",
		Network:          network,
		Gateway:          gateway,
		TapMAC:           "02:00:00:00:00:00",
		Device:           tap.NewFake("srv"),
		ManagementListen: "-",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if addr := srv.ManagementAddr(); addr != "" {
		t.Fatalf("禁用管理端口时不应监听: %s", addr)
	}
	srv.Stop()
}

// TestManagementListenRetry - 端口被占用时 manageLoop 应在检测间隔内重试，
// 占用释放后恢复可查询（TCP 化后取代 unix socket 自愈测试）。
func TestManagementListenRetry(t *testing.T) {
	fastManagementWatch(t)

	// 预占一个回环端口
	occ, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := occ.Addr().String()

	network, gateway := NewTestNetwork()
	srv, err := New(Config{
		Listen:           "127.0.0.1:0",
		Network:          network,
		Gateway:          gateway,
		TapMAC:           "02:00:00:00:00:00",
		Device:           tap.NewFake("srv"),
		ManagementListen: addr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Stop() })
	time.Sleep(150 * time.Millisecond) // 确保已尝试并失败一次
	if a := srv.ManagementAddr(); a != "" {
		t.Fatalf("端口被占用时不应监听成功: %s", a)
	}

	occ.Close() // 释放占用，触发重试重建
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if srv.ManagementAddr() != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if a := srv.ManagementAddr(); a == "" {
		t.Fatal("占用释放后管理端口未重建")
	}
	mgmtQuery(t, srv.ManagementAddr())
}

// TestManagementStopClosesPort - Stop 后管理端口立即不可连接。
func TestManagementStopClosesPort(t *testing.T) {
	network, gateway := NewTestNetwork()
	srv, err := New(Config{
		Listen:           "127.0.0.1:0",
		Network:          network,
		Gateway:          gateway,
		TapMAC:           "02:00:00:00:00:00",
		Device:           tap.NewFake("srv"),
		ManagementListen: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	waitManagementReady(t, srv)
	addr := srv.ManagementAddr()
	srv.Stop()

	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err == nil {
		conn.Close()
		t.Fatalf("Stop 后管理端口应已关闭: %s", addr)
	}
}
