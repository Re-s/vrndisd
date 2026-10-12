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
	TypePing       byte = 6
	TypePong       byte = 7
	TypeManagement byte = 8
	// 新增 P2P 消息类型
	TypeP2PAdvertise  byte = 9  // 客户端 -> 服务器：上报本机候选（NAT 映射地址）
	TypePeerCandidate byte = 10 // 服务器 -> 客户端：下发对端 MAC + 候选地址
)

// MaxBody 单条消息体上限（256 KiB，容纳超长帧）。
const MaxBody = 256 * 1024

// ProtocolVersion 当前协议版本。
//
//	1: 纯中继（服务器经 TCP + 学习型交换机转发所有帧）
//	2: 加入 P2P 直连（客户端上报候选、服务器协调交换、打洞失败回退中继）
const ProtocolVersion = 2

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
	case TypeP2PAdvertise:
		return "P2P_ADV"
	case TypePeerCandidate:
		return "PEER_CAND"
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

// 管理消息
type ManagementRequest struct {
	Cmd string `json:"cmd"`
}

type ManagementResponse struct {
	Version string       `json:"version"`
	Clients int          `json:"clients"`
	List    []ClientInfo `json:"list,omitempty"`
}

// CustomClientInfo for JSON
type ClientInfo struct {
	IP   string `json:"ip"`
	MAC  string `json:"mac"`
	Peer string `json:"peer"`
	Name string `json:"name"`
}

func (ci ClientInfo) Serialize() []byte {
	data, _ := json.Marshal(ci)
	return data
}

// P2PAdvertise 客户端向服务器上报本机 NAT 候选（P2P 模式）。
type P2PAdvertise struct {
	Version   int    `json:"version"`   // 协议版本，用于兼容
	MAC       string `json:"mac"`       // 本机虚拟 MAC
	Candidate string `json:"candidate"` // "host:port"，NAT 映射后的公网地址
}

// PeerCandidate 服务器向客户端下发对端信息（含对端 MAC + 候选地址）。
type PeerCandidate struct {
	Version   int    `json:"version"`
	PeerMAC   string `json:"peer_mac"`  // 对端虚拟 MAC
	Candidate string `json:"candidate"` // 对端 "host:port"
}
