// Package crypto 握手协议。
//
// 握手在 TCP 连接建立之后、任何 proto.Message 之前完成。
// 握手完成后，双方各自持有 writeKey/readKey，后续所有消息经 SecureConn 加密。
//
// 协议：
//
//  1. 客户端 -> 服务器：E_KC（32 byte 临时 X25519 公钥）
//  2. 服务器 -> 客户端：E_KS（32 byte） || MAC（16 byte HMAC-SHA256 截断）
//     MAC = HMAC-PSK(E_KC || E_KS) 的前 16 字节
//  3. 双方各自用 PSKHandshake 派生密钥
//
// 认证方向：客户端验证服务器的 MAC。服务器不单独验证客户端（PSK 本身
// 即认证：不知道 PSK 的客户端无法派生出正确的密钥，后续加密流量无法解密）。
//
// 错误处理：握手失败立即关闭连接，不泄露任何信息。
package crypto

import (
	"crypto/hmac"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"

	"github.com/Re-s/vrndisd/internal/proto"
)

// CryptoHello 握手第一条消息的 body（线路格式：len | type | body）。
type CryptoHello struct {
	Version int    `json:"version"` // 协议版本，用于兼容
	Pub     string `json:"pub"`     // base64 编码的临时公钥
}

// CryptoAck 握手第二条消息的 body。
type CryptoAck struct {
	Version int    `json:"version"`
	Pub     string `json:"pub"`
	Mac     string `json:"mac"` // base64 编码的 HMAC
}

// ErrHandshake 握手失败。
var ErrHandshake = errors.New("github.com/Re-s/vrndisd/crypto: handshake failed")

// ClientHandshake 作为客户端完成握手。
//
// 流程：发送 E_KC -> 接收 E_KS+MAC -> 验证 MAC -> 派生密钥。
// 返回的 SecureConn 已配置好 writeKey/readKey（客户端写 = 服务器读）。
func ClientHandshake(conn net.Conn, psk []byte, version int) (*SecureConn, error) {
	c, err := NewKeyPair(rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := writeJSON(conn, proto.TypeCryptoHello, CryptoHello{
		Version: version,
		Pub:     base64.StdEncoding.EncodeToString(c.Public()),
	}); err != nil {
		return nil, err
	}

	var ack CryptoAck
	if _, err := readTyped(conn, &ack, proto.TypeCryptoAck); err != nil {
		return nil, err
	}
	serverPub, err := base64.StdEncoding.DecodeString(ack.Pub)
	if err != nil {
		return nil, err
	}
	expectedMac, err := base64.StdEncoding.DecodeString(ack.Mac)
	if err != nil {
		return nil, err
	}

	keys, mac, err := c.PSKHandshake(serverPub, psk, c.Public(), true)
	if err != nil {
		return nil, err
	}
	if !hmac.Equal(expectedMac, mac) {
		return nil, ErrHandshake
	}
	return NewSecureConn(conn, keys[0], keys[1])
}

// ServerHandshake 作为服务器完成握手。
//
// 流程：接收 E_KC -> 回复 E_KS+MAC -> 派生密钥。
// 返回的 SecureConn 已配置好 writeKey/readKey（服务器写 = 客户端读）。
func ServerHandshake(conn net.Conn, psk []byte, version int) (*SecureConn, error) {
	var hello CryptoHello
	if _, err := readTyped(conn, &hello, proto.TypeCryptoHello); err != nil {
		return nil, err
	}
	clientPub, err := base64.StdEncoding.DecodeString(hello.Pub)
	if err != nil {
		return nil, err
	}

	c, err := NewKeyPair(rand.Reader)
	if err != nil {
		return nil, err
	}
	keys, mac, err := c.PSKHandshake(clientPub, psk, c.Public(), false)
	if err != nil {
		return nil, err
	}
	if err := writeJSON(conn, proto.TypeCryptoAck, CryptoAck{
		Version: version,
		Pub:     base64.StdEncoding.EncodeToString(c.Public()),
		Mac:     base64.StdEncoding.EncodeToString(mac),
	}); err != nil {
		return nil, err
	}
	return NewSecureConn(conn, keys[0], keys[1])
}

// writeJSON 编码 JSON 并通过线路协议发送。
func writeJSON(conn net.Conn, typ byte, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return proto.WriteMessage(conn, typ, data)
}

// readTyped 读取一条消息并断言其类型，否则返回 ErrHandshake。
func readTyped(conn net.Conn, v any, want byte) (byte, error) {
	msg, err := proto.ReadMessage(conn)
	if err != nil {
		return 0, err
	}
	if msg.Type != want {
		// 类型不符通常意味着对端没有发起加密握手：最常见的根因是双方
		// PSK 配置不一致（一端带 --psk、另一端没带）。把期望/实际类型
		// 以及可操作提示一并写入错误，省得用户对着 EOF 猜原因。
		return 0, fmt.Errorf("%w: 期望 %s，收到 %s（对端未启用加密握手？双方 --psk 需一致）",
			ErrHandshake, proto.TypeName(want), proto.TypeName(msg.Type))
	}
	if err := json.Unmarshal(msg.Body, v); err != nil {
		return 0, err
	}
	return msg.Type, nil
}
