package main

import (
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Re-s/vrndisd/internal/proto"
)

// fakeMgmtServer 启动管理端口模拟服务器：接受连接后读取 ManagementRequest，
// 回 state JSON（模拟 server.handleManagementConn 行为）。返回监听地址。
func fakeMgmtServer(t *testing.T, state proto.ManagementState) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
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

	return ln.Addr().String()
}

// TestQueryManagementClients - clients 查询应正确解析管理端口返回的状态快照。
func TestQueryManagementClients(t *testing.T) {
	state := proto.ManagementState{
		Version:   "1.0.0",
		UpdatedAt: time.Now(),
		Clients: []proto.ClientInfo{
			{Name: "pc-a", IP: "10.77.0.2", MAC: "02:00:00:00:00:01", Peer: "1.2.3.4:5"},
		},
	}
	addr := fakeMgmtServer(t, state)

	got, err := queryManagement(addr, "clients")
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
	addr := fakeMgmtServer(t, state)

	got, err := queryManagement(addr, "clients")
	if err != nil {
		t.Fatalf("queryManagement err=%v", err)
	}
	if len(got.Clients) != 0 {
		t.Fatalf("应为空列表: got=%+v", got)
	}
}

// TestQueryManagementDefaultAddr - addr 为空时回退默认地址，应给出包含
// 默认地址与运行提示的明确错误（而非空指针/EOF）。
// 注意：本机若正运行 vrndisd 服务器（如开发机即生产机），7790 会连通，
// 此时跳过错误文案校验。
func TestQueryManagementDefaultAddr(t *testing.T) {
	if conn, err := net.DialTimeout("tcp", defaultManagementAddr(), 500*time.Millisecond); err == nil {
		conn.Close()
		t.Skipf("默认地址 %s 正被占用（本机在跑服务器），跳过错误文案校验", defaultManagementAddr())
	}
	_, err := queryManagement("", "clients")
	if err == nil {
		t.Fatal("应报错")
	}
	if msg := err.Error(); !strings.Contains(msg, "服务器是否在运行") || !strings.Contains(msg, defaultManagementAddr()) {
		t.Fatalf("错误提示不明确: %s", msg)
	}
}

// TestCmdLocalAddr - local --addr 单查：client 实例应打印 P2P 状态概要。
func TestCmdLocalAddr(t *testing.T) {
	state := proto.ManagementState{
		Version:   "1.0.0",
		UpdatedAt: time.Now(),
		Role:      "client",
		Client: &proto.ClientStatus{
			Name:       "x16",
			Server:     "203.0.113.1:7777",
			Connected:  true,
			Connects:   3,
			VirtualIP:  "10.77.0.3",
			VirtualMAC: "02:00:00:00:00:03",
			P2P: proto.ClientP2PStatus{
				Enabled:   true,
				Active:    true,
				PeerMAC:   "02:00:00:00:00:05",
				Remote:    "192.168.1.100:54321",
				LastRxAgo: "2s",
				LastTxAgo: "1s",
			},
		},
	}
	addr := fakeMgmtServer(t, state)
	if err := cmdLocal([]string{"--addr", addr}); err != nil {
		t.Fatalf("cmdLocal err=%v", err)
	}
}

// TestCmdLocalLegacyServer - 旧版服务器无 Role 字段时按 server 打印（兼容）。
func TestCmdLocalLegacyServer(t *testing.T) {
	state := proto.ManagementState{
		Version:   "1.0.0",
		UpdatedAt: time.Now(),
		Clients:   []proto.ClientInfo{{Name: "pc-a", IP: "10.77.0.2"}},
	}
	addr := fakeMgmtServer(t, state)
	if err := cmdLocal([]string{"--addr", addr}); err != nil {
		t.Fatalf("cmdLocal err=%v", err)
	}
}
