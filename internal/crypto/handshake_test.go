package crypto

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"vrndis/internal/proto"
)

// PSK 配置不一致是最常见的接入失败：一端带 --psk、另一端没带。
// 带 PSK 的一端会把对端发来的第一条消息当作握手包读取，此时错误必须
// 指明期望/实际类型并给出可操作提示，而不是一个光秃秃的 handshake failed。

func TestHandshakeTypeMismatch(t *testing.T) {
	a, b := newPair()
	defer a.Close()
	defer b.Close()

	// 模拟未带 PSK 的客户端：跳过握手，直接发明文 HELLO。
	hello, _ := json.Marshal(proto.Hello{Version: proto.ProtocolVersion, Name: "no-psk"})
	if err := proto.WriteMessage(a, proto.TypeHello, hello); err != nil {
		t.Fatalf("write hello: %v", err)
	}

	errCh := make(chan error, 1)
	go func() {
		_, err := ServerHandshake(b, []byte("server-psk"), proto.ProtocolVersion)
		errCh <- err
	}()
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("带 PSK 的服务器应拒绝明文连接")
		}
		if !errors.Is(err, ErrHandshake) {
			t.Fatalf("应以 ErrHandshake 为基错: got %v", err)
		}
		msg := err.Error()
		for _, want := range []string{"CRYPTO_HELLO", "HELLO", "对端未启用加密握手"} {
			if !strings.Contains(msg, want) {
				t.Fatalf("错误信息应包含 %q: got %q", want, msg)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ServerHandshake 未返回")
	}
}
