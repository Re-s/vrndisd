package main

import (
	"encoding/json"
	"net"
	"os"
	"strings"
	"path/filepath"
	"testing"
	"time"

	"vrndis/internal/proto"
)

// fakeMgmtSocket 启动管理套接字模拟服务器：接受连接后读取 ManagementRequest，
// 回 state JSON（模拟 server.handleManagementConn 行为）。
func fakeMgmtSocket(t *testing.T, state proto.ManagementState) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "management.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		var req proto.ManagementRequest
		if err := json.NewDecoder(conn).Decode(&req); err != nil {
			return
		}
		json.NewEncoder(conn).Encode(state)
	}()

	return path
}

// TestQueryManagementClients - clients 查询应正确解析套接字返回的状态快照。
func TestQueryManagementClients(t *testing.T) {
	state := proto.ManagementState{
		Version:   "1.0.0",
		UpdatedAt: time.Now(),
		Clients: []proto.ClientInfo{
			{Name: "pc-a", IP: "10.77.0.2", MAC: "02:00:00:00:00:01", Peer: "1.2.3.4:5"},
		},
	}
	path := fakeMgmtSocket(t, state)

	got, err := queryManagement(path, "clients")
	if err != nil {
		t.Fatalf("queryManagement err=%v", err)
	}
	if got.Version != "1.0.0" || len(got.Clients) != 1 {
		t.Fatalf("解析错误: got=%+v", got)
	}
	c := got.Clients[0]
	if c.Name != "pc-a" || c.MAC != "02:00:00:00:00:01" || c.Peer != "1.2.3.4:5" {
		t.Fatalf("ClientInfo 解析错误: %+v", c)
	}
	if c.IP != "10.77.0.2" {
		t.Fatalf("IP 解析错误: %s", c.IP)
	}
}

// TestQueryManagementEmpty - 无在线客户端时 clients 为空列表，不报错。
func TestQueryManagementEmpty(t *testing.T) {
	state := proto.ManagementState{Version: "1.0.0", Clients: []proto.ClientInfo{}}
	path := fakeMgmtSocket(t, state)

	got, err := queryManagement(path, "clients")
	if err != nil {
		t.Fatalf("queryManagement err=%v", err)
	}
	if len(got.Clients) != 0 {
		t.Fatalf("应为空列表: got=%+v", got)
	}
}

// TestQueryManagementNoServer - 套接字不存在时应给出明确错误（而非 EOF/broken pipe）。
func TestQueryManagementNoServer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.sock")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Skip("意外存在")
	}
	_, err := queryManagement(path, "clients")
	if err == nil {
		t.Fatal("应报错")
	}
	if msg := err.Error(); !strings.Contains(msg, "服务器是否在运行") {
		t.Fatalf("错误提示不明确: %s", msg)
	}
}
