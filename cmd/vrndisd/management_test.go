package main

import (
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"vrndis/internal/proto"
)

// fakeMgmtServer 启动一个模拟服务器：接受连接，先发 TypeAssign 握手（模拟真实
// server.handleConn 的行为），随后处理 TypeManagement 请求并回 ManagementResponse。
// sendExtras: 在 Assign 与 Management 响应之间再插入的非管理消息（模拟心跳/HELLO 应答等）。
func fakeMgmtServer(t *testing.T, resp proto.ManagementResponse, sendExtras ...proto.Message) (addr string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))

		// 模拟真实服务器：接受连接即下发 TypeAssign 握手（Bug 3 根因）。
		assign := proto.Assignment{IP: "10.77.0.2", Prefix: 24, Gateway: "10.77.0.1"}
		ab, _ := json.Marshal(assign)
		if err := proto.WriteMessage(conn, proto.TypeAssign, ab); err != nil {
			return
		}
		// 先读客户端的 Hello 握手。
		if _, err := proto.ReadMessage(conn); err != nil {
			return
		}
		// 处理 ManagementRequest。
		if _, err := proto.ReadMessage(conn); err != nil {
			return
		}
		// 插入可选的非管理消息（心跳等），应被客户端跳过。
		for _, m := range sendExtras {
			if err := proto.WriteMessage(conn, m.Type, m.Body); err != nil {
				return
			}
		}
		rb, _ := json.Marshal(resp)
		proto.WriteMessage(conn, proto.TypeManagement, rb)
	}()

	return ln.Addr().String()
}

// TestSendManagementRequestSkipsAssignHandshake - 服务器先发 Assign 握手，再回
// Management 响应。修复后应跳过 Assign，正确拿到 ManagementResponse。
func TestSendManagementRequestSkipsAssignHandshake(t *testing.T) {
	resp := proto.ManagementResponse{
		Version: "1.0.0",
		Clients: 1,
		List: []proto.ClientInfo{
			{Name: "pc-a", IP: "10.77.0.2", MAC: "02:00:00:00:00:01", Peer: "1.2.3.4:5"},
		},
	}
	addr := fakeMgmtServer(t, resp)

	got, err := sendManagementRequest(addr, "clients")
	if err != nil {
		t.Fatalf("sendManagementRequest err=%v", err)
	}
	if got.Clients != 1 || len(got.List) != 1 {
		t.Fatalf("解析错误: got=%+v", got)
	}
	c := got.List[0]
	if c.Name != "pc-a" || c.MAC != "02:00:00:00:00:01" || c.Peer != "1.2.3.4:5" {
		t.Fatalf("ClientInfo 解析错误: %+v", c)
	}
}

// TestSendManagementRequestSkipsNonManagement - 服务器在 Assign 后、Management 响应前
// 插入 TypePing 等非管理消息，应被跳过而非误报“收到非管理消息类型”。
func TestSendManagementRequestSkipsNonManagement(t *testing.T) {
	resp := proto.ManagementResponse{Version: "1.0.0", Clients: 0}
	addr := fakeMgmtServer(t, resp,
		proto.Message{Type: proto.TypePing},
		proto.Message{Type: proto.TypeAssign, Body: []byte(`{}`)})

	got, err := sendManagementRequest(addr, "status")
	if err != nil {
		t.Fatalf("sendManagementRequest err=%v", err)
	}
	if got.Version != "1.0.0" {
		t.Fatalf("Version=%q want 1.0.0", got.Version)
	}
}

// TestCmdClientsPrintsList - cmdClients 通过真实 TCP 往返打印在线客户端列表。
func TestCmdClientsPrintsList(t *testing.T) {
	resp := proto.ManagementResponse{
		Clients: 2,
		List: []proto.ClientInfo{
			{Name: "a", IP: "10.77.0.2", MAC: "02:00:00:00:00:01", Peer: "1.1.1.1:5"},
			{Name: "b", IP: "10.77.0.3", MAC: "02:00:00:00:00:02", Peer: "2.2.2.2:6"},
		},
	}
	addr := fakeMgmtServer(t, resp)

	out := captureStdout(t, func() {
		if err := cmdClients([]string{"--server", addr}); err != nil {
			t.Errorf("cmdClients err=%v", err)
		}
	})
	if !strings.Contains(out, "在线客户端列表 (2)") {
		t.Errorf("缺少列表头: %q", out)
	}
	if !strings.Contains(out, "a") || !strings.Contains(out, "b") {
		t.Errorf("缺少客户端条目: %q", out)
	}
}

// TestSendManagementRequestNoResponse - 服务器不回 Management 响应时返回“未收到管理响应”。
func TestSendManagementRequestNoResponse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	// 服务器收到连接后只发 Assign，不回 Management，直接断开。
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		proto.WriteMessage(conn, proto.TypeAssign, []byte(`{}`))
		conn.Read(make([]byte, 1))
	}()

	_, err = sendManagementRequest(ln.Addr().String(), "status")
	// 服务器不回 Management 响应即关闭连接：无论在哪一步报错，都必须返回错误而非成功。
	if err == nil {
		t.Fatalf("期望错误（服务器未回管理响应），got=nil")
	}
}
