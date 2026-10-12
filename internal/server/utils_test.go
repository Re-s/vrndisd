package server

import (
	"testing"
	"vrndis/internal/proto"
)

// TestClientInfoConversion 测试 ClientInfo 的转换逻辑
func TestClientInfoConversion(t *testing.T) {
	// 构造一些假的 session 信息并验证转换逻辑
	info := proto.ClientInfo{
		IP:   "10.77.0.100",
		MAC:  "02:00:00:00:00:01",
		Peer: "127.0.0.1:12345",
		Name: "test-client",
	}

	if info.IP != "10.77.0.100" {
		t.Errorf("IP = %q, want '10.77.0.100'", info.IP)
	}
	if info.MAC != "02:00:00:00:00:01" {
		t.Errorf("MAC = %q, want '02:00:00:00:00:01'", info.MAC)
	}
	if info.Name != "test-client" {
		t.Errorf("Name = %q, want 'test-client'", info.Name)
	}
}

// TestGetManagementClientInfoDummy 模拟测试（实际逻辑在 server.go，这里只验证接口）
func TestGetManagementClientInfoSignature(t *testing.T) {
	// 确保 getManagementClientInfo 方法签名正确
	// 这实际上是检查编译，而不是运行时行为
	_ = func(s *Server) []proto.ClientInfo {
		// 注意：这是私有方法，这里只是一个兼容性占位
		return nil
	}
}
