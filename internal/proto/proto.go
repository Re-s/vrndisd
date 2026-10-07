// Package proto 定义 vrndis 线路协议。
//
// TCP 连接上的每条消息：body_len(u32 大端) | type(u8) | body。
// 控制消息（HELLO/ASSIGN/KEEPALIVE/BYE）的 body 为 JSON；
// DATA 消息的 body 为原始以太网帧。
package proto

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	TypeHello     byte = 1
	TypeAssign    byte = 2
	TypeData      byte = 3
	TypeKeepalive byte = 4
	TypeBye       byte = 5
	// 新增管理消息类型
	TypePing      byte = 6
	TypePong      byte = 7
	TypeManagement byte = 8
)

// MaxBody 单条消息体上限（256 KiB，容纳超长帧）。
const MaxBody = 256 * 1024

// ProtocolVersion 当前协议版本。
const ProtocolVersion = 1

var (
	ErrTooLarge   = errors.New("vrndis/proto: message body too large")
	ErrBadMessage = errors.New("vrndis/proto: malformed message")
)

// Message 一条线路消息。
type Message struct {
	Type byte
	Body []byte
}

// TypeName 用于日志的消息名。
func TypeName(t byte) string {
	switch t {
	case TypeHello:
		return "HELLO"
	case TypeAssign:
		return "ASSIGN"
	case TypeData:
		return "DATA"
	case TypeKeepalive:
		return "KEEPALIVE"
	case TypeBye:
		return "BYE"
	case TypePing:
		return "PING"
	case TypePong:
		return "PONG"
	case TypeManagement:
		return "MANAGEMENT"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", t)
	}
}

// WriteMessage 把一条消息完整写入 w。调用方负责并发互斥。
func WriteMessage(w io.Writer, typ byte, body []byte) error {
	if len(body) > MaxBody {
		return ErrTooLarge
	}
	hdr := make([]byte, 5)
	binary.BigEndian.PutUint32(hdr[:4], uint32(len(body)))
	hdr[4] = typ
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	if len(body) > 0 {
		if _, err := w.Write(body); err != nil {
			return err
		}
	}
	return nil
}

// ReadMessage 读取一条消息；对端断开时返回 io.EOF / io.ErrUnexpectedEOF。
func ReadMessage(r io.Reader) (Message, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Message{}, err
	}
	n := binary.BigEndian.Uint32(hdr[:4])
	if n > MaxBody {
		return Message{}, fmt.Errorf("%w: %d > %d", ErrTooLarge, n, MaxBody)
	}
	body := make([]byte, n)
	if n > 0 {
		if _, err := io.ReadFull(r, body); err != nil {
			return Message{}, err
		}
	}
	return Message{Type: hdr[4], Body: body}, nil
}

// WriteJSON 编码 JSON 控制消息。
func WriteJSON(w io.Writer, typ byte, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return WriteMessage(w, typ, data)
}

// ReadJSON 读取消息并解析 JSON body。
func ReadJSON(r io.Reader, v any) (byte, error) {
	msg, err := ReadMessage(r)
	if err != nil {
		return 0, err
	}
	if err := json.Unmarshal(msg.Body, v); err != nil {
		return msg.Type, fmt.Errorf("%w: %v", ErrBadMessage, err)
	}
	return msg.Type, nil
}
