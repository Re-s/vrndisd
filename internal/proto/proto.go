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
	"time"
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
	TypeP2PReady      byte = 13 // 客户端 -> 服务器：打洞成功，数据面已切 UDP 直连
	TypePeerQuery     byte = 14 // 客户端 -> 服务器：查询 peer 候选（ping/status 用）
	TypePeerCandidate byte = 10 // 服务器 -> 客户端：下发对端 MAC + 候选地址
	// 加密握手消息类型（v3 协议，在 HELLO/ASSIGN 之前协商）
	TypeCryptoHello byte = 11 // 客户端 -> 服务器：E_KC + 版本号
	TypeCryptoAck   byte = 12 // 服务器 -> 客户端：E_KS + HMAC-PSK
)

// MaxBody 单条消息体上限（256 KiB，容纳超长帧）。
const MaxBody = 256 * 1024

// ProtocolVersion 当前协议版本。
//
//	1: 纯中继（服务器经 TCP + 学习型交换机转发所有帧）
//	2: 加入 P2P 直连（客户端上报候选、服务器协调交换、打洞失败回退中继）
//	3: 加入线路加密（PSK + 一次性 ECDH 握手，AES-256-GCM 数据面加密）
const ProtocolVersion = 3

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
	case TypeP2PReady:
		return "P2P_READY"
	case TypePeerQuery:
		return "PEER_QUERY"
	case TypePeerCandidate:
		return "PEER_CAND"
	case TypeCryptoHello:
		return "CRYPTO_HELLO"
	case TypeCryptoAck:
		return "CRYPTO_ACK"
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

// ManagementState 本地管理状态文件（替代 clients 命令 TCP 自连查询）：
// 服务器在启停与客户端上下线时写入，vrndisd clients/status 直接读取。
type ManagementState struct {
	Version   string       `json:"version"`
	UpdatedAt time.Time    `json:"updated_at"`
	Clients   []ClientInfo `json:"clients"`
}

// CustomClientInfo for JSON
type ClientInfo struct {
	IP   string `json:"ip"`
	MAC  string `json:"mac"`
	Peer string `json:"peer"`
	Name string `json:"name"`
	P2P  string `json:"p2p"` // P2P 状态：off / relay / stun-ok <候选> / p2p-ok <对端MAC>
}

func (ci ClientInfo) Serialize() []byte {
	data, _ := json.Marshal(ci)
	return data
}

// PeerQuery 查询 peer 候选地址（客户端 -> 服务器）。
// Target 为对端 IP 或 MAC；空表示请求全部已知 peer。
// 服务器以逐条 TypePeerCandidate 应答，无匹配时回一条空 Candidate。
type PeerQuery struct {
	Version int    `json:"version"`
	Target  string `json:"target"`
}

// P2PReady 打洞成功通知（客户端 -> 服务器）。
// 服务器据此在管理面（clients/status）展示该会话已切换 UDP 直连。
type P2PReady struct {
	Version int    `json:"version"`  // 协议版本，用于兼容
	PeerMAC string `json:"peer_mac"` // 对端虚拟 MAC
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
	PeerIP    string `json:"peer_ip"`   // 对端虚拟 IP（诊断展示用）
	PeerName  string `json:"peer_name"` // 对端名称（诊断展示用）
	Candidate string `json:"candidate"` // 对端 "host:port"
}
