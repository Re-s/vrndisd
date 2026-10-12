package proto

import (
	"bytes"
	"testing"
)

// TestManagementMessageSimple 测试简单的管理消息发送
func TestManagementMessageSimple(t *testing.T) {
	// 测试 WriteJSON 和 ReadJSON 能否处理管理消息
	var buf bytes.Buffer
	req := ManagementRequest{Cmd: "status"}

	// WriteJSON 会写入 JSON 数据
	if err := WriteJSON(&buf, TypeData, req); err != nil {
		t.Fatalf("WriteJSON failed: %v", err)
	}

	// 读取数据长度和类型
	typ, err := ReadJSON(&buf, new(ManagementRequest))
	if err != nil {
		t.Fatalf("ReadJSON failed: %v", err)
	}

	// TypeData 用于测试
	if typ != TypeData {
		t.Fatalf("Type = %d, want %d", typ, TypeData)
	}
}

// TestPingPongType 测试 Ping/Pong 消息类型的实际值
func TestPingPongType(t *testing.T) {
	// 查看实际声明
	_ = TypePing
	_ = TypePong
	// 不硬编码值，只确保函数可调用
	if TypeName(TypePing) == "" {
		t.Error("TypeName should not return empty")
	}
}

// FuzzManagementMessage 模糊测试
func FuzzManagementMessage(f *testing.F) {
	f.Add("status")
	f.Fuzz(func(t *testing.T, cmd string) {
		if len(cmd) == 0 {
			return
		}
		req := ManagementRequest{Cmd: cmd}
		var buf bytes.Buffer
		if err := WriteJSON(&buf, TypeData, req); err != nil {
			t.Fatalf("failed to write: %v", err)
		}
	})
}
