package server

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"vrndis/internal/proto"
	"vrndis/internal/tap"
)

// mgmtTestServer 启动一个带管理套接字的测试服务器。
func mgmtTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	network, gateway := NewTestNetwork()
	srv, err := New(Config{
		Listen:           "127.0.0.1:0",
		Network:          network,
		Gateway:          gateway,
		TapMAC:           "02:00:00:00:00:00",
		Device:           tap.NewFake("srv"),
		ManagementSocket: filepath.Join(t.TempDir(), "management.sock"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	path := srv.managementSocketPath()
	t.Cleanup(func() { srv.Stop() })
	waitManagementReady(t, path)
	return srv, path
}

func waitManagementReady(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("管理套接字未就绪: %s", path)
}

// mgmtQuery 发一次管理查询并返回状态快照。
func mgmtQuery(t *testing.T, path string) proto.ManagementState {
	t.Helper()
	conn, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		t.Fatalf("连接管理套接字 %s 失败: %v", path, err)
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
	orig := managementWatchInterval
	managementWatchInterval = 50 * time.Millisecond
	t.Cleanup(func() { managementWatchInterval = orig })
}

// TestManagementSocketSelfHeal 验证 socket 文件被外部删除后
// manageLoop 在检测间隔内重建并恢复可查询。
func TestManagementSocketSelfHeal(t *testing.T) {
	fastManagementWatch(t)
	_, path := mgmtTestServer(t)
	mgmtQuery(t, path) // 基线可用

	if err := os.Remove(path); err != nil {
		t.Fatalf("删除 socket 失败: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("管理套接字未自愈重建: %v", err)
	}
	mgmtQuery(t, path) // 自愈后仍可用
}

// TestManagementSocketStopKeepsFile 验证 Stop 不删除 socket 文件（50cacf1 语义）。
func TestManagementSocketStopKeepsFile(t *testing.T) {
	fastManagementWatch(t)
	srv, path := mgmtTestServer(t)

	srv.Stop()
	time.Sleep(150 * time.Millisecond)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Stop 后 socket 文件应保留（供下次启动清理）: %v", err)
	}
}

// TestManagementSocketSelfHealRepeat 验证可连续多次自愈。
func TestManagementSocketSelfHealRepeat(t *testing.T) {
	fastManagementWatch(t)
	_, path := mgmtTestServer(t)
	for i := 0; i < 3; i++ {
		mgmtQuery(t, path)
		if err := os.Remove(path); err != nil {
			t.Fatalf("第 %d 次删除失败: %v", i, err)
		}
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(path); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	mgmtQuery(t, path)
}
