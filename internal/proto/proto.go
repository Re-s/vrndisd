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
	TypeP2PWithdraw   byte = 15 // 客户端 -> 服务器：撤销本机候选（打洞失败/回退中继）
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
	case TypeP2PWithdraw:
		return "P2P_WITHDRAW"
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
	Version   string        `json:"version"`
	UpdatedAt time.Time     `json:"updated_at"`
	Role      string        `json:"role,omitempty"`   // server | client；旧版服务器为空，查询方按 server 处理
	Clients   []ClientInfo  `json:"clients"`          // server: 在线客户端列表
	Client    *ClientStatus `json:"client,omitempty"` // client: 本实例状态
}

// ClientStatus 客户端实例自身状态（client 本机管理面专用）。
type ClientStatus struct {
	Name       string          `json:"name"`
	Server     string          `json:"server"`
	Connected  bool            `json:"connected"`
	Connects   int64           `json:"connects"` // 成功建连次数（含首次）
	VirtualIP  string          `json:"virtual_ip,omitempty"`
	VirtualMAC string          `json:"virtual_mac,omitempty"`
	Gateway    string          `json:"gateway,omitempty"`
	StartedAt  time.Time       `json:"started_at"`
	P2P        ClientP2PStatus `json:"p2p"`
}

// ClientP2PStatus P2P 直连状态（诊断打洞失败 / 隧道空闲关闭用）。
type ClientP2PStatus struct {
	Enabled   bool              `json:"enabled"`
	Active    bool              `json:"active"`                // 隧道存活
	PeerMAC   string            `json:"peer_mac,omitempty"`    // 对端虚拟 MAC
	Local     string            `json:"local,omitempty"`       // 本端 UDP 地址
	Remote    string            `json:"remote,omitempty"`      // 对端 UDP 地址
	LastRxAgo string            `json:"last_rx_ago,omitempty"` // 距上次收到数据
	LastTxAgo string            `json:"last_tx_ago,omitempty"` // 距上次发出数据
	LastFail  string            `json:"last_fail,omitempty"`   // 上一轮打洞失败原因
	Routes    map[string]string `json:"routes,omitempty"`      // 已直连对端 MAC -> UDP 地址
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

// P2PWithdraw 撤销本机候选（客户端 -> 服务器）。
// 打洞失败、等待候选超时或回退中继时发送：服务器据此从 peer 表移除该
// 候选，避免死候选残留导致后来节点向已关闭的端点空打洞。
type P2PWithdraw struct {
	Version int    `json:"version"` // 协议版本，用于兼容
	MAC     string `json:"mac"`     // 本机虚拟 MAC
}

// P2PAdvertise 客户端向服务器上报本机 NAT 候选（P2P 模式）。
//
// 候选多路化（提升网状直连成功率）：
//   - Candidate：srflx 主候选（STUN 公网映射），兼容旧服务器
//   - Candidates：全部候选，顺序为 srflx、UPnP/NAT-PMP 映射、LAN host（v4 私网）、
//     公网 IPv6。服务器按接收方与提供者的关系过滤后下发（内网候选只给
//     同出口 peer），客户端并发向全部候选打洞，先通者胜。
//
// 兼容：旧服务器只读 Candidate（行为同单候选时代）；旧客户端忽略 Candidates。
type P2PAdvertise struct {
	Version    int      `json:"version"`              // 协议版本，用于兼容
	MAC        string   `json:"mac"`                  // 本机虚拟 MAC
	Candidate  string   `json:"candidate"`            // "host:port"，NAT 映射后的公网主候选（兼容）
	Candidates []string `json:"candidates,omitempty"` // 全部候选（含主候选）
}

// PeerCandidate 服务器向客户端下发对端信息（含对端 MAC + 候选地址）。
//
// Candidates 为对端全部候选（经服务器按本机与对端的网络关系过滤），
// 旧客户端只读 Candidate（对端首个公网候选），新客户端应优先用 Candidates。
type PeerCandidate struct {
	Version    int      `json:"version"`
	PeerMAC    string   `json:"peer_mac"`             // 对端虚拟 MAC
	PeerIP     string   `json:"peer_ip"`              // 对端虚拟 IP（诊断展示用）
	PeerName   string   `json:"peer_name"`            // 对端名称（诊断展示用）
	Candidate  string   `json:"candidate"`            // 对端 "host:port"（兼容：首个公网候选）
	Candidates []string `json:"candidates,omitempty"` // 对端全部候选
}
