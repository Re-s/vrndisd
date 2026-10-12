package protocol

import (
	"testing"
)

// TestBindingRequestResponseRoundTrip 测试 Binding 响应的基本解析
func TestBindingRequestResponseRoundTrip(t *testing.T) {
	data := make([]byte, 24)
	data[0] = 0x00
	data[1] = 0x01
	data[2] = 0x00
	data[3] = 0x00
	copy(data[4:8], []byte{0x21, 0x12, 0xA4, 0x42})
	for i := 0; i < 16; i++ {
		data[8+i] = byte(i)
	}

	_, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
}

// TestUnknownMessageParsing 测试未知类型
func TestUnknownMessageParsing(t *testing.T) {
	data := []byte{0xFF, 0x01, 0x00, 0x00, 0x21, 0x12, 0xA4, 0x42, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	_, err := Parse(data)
	if err != nil {
		return
	}
}

// FuzzParse 模糊测试
func FuzzParse(f *testing.F) {
	f.Add([]byte{0x00, 0x01, 0x00, 0x00, 0x21, 0x12, 0xA4, 0x42, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		_, err := Parse(data)
		if err != nil {
			return
		}
	})
}

// TestMagicCookieValue 确认 magic cookie 值
func TestMagicCookieValue(t *testing.T) {
	const magic = 0x2112A442
	if MagicCookie != magic {
		t.Fatalf("MagicCookie = %d, want %d", MagicCookie, magic)
	}
}
