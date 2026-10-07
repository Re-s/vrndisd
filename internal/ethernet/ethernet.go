// Package ethernet 提供以太网帧与 ARP 的最小解析/构造能力。
// 虚拟 RNDIS 工作在二层，ARP/IP 等三层协议交由两端内核处理。
package ethernet

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
)

const (
	HeaderLen      = 14
	TypeIPv4       = 0x0800
	TypeARP        = 0x0806
	TypeIPv6       = 0x86DD
	ARPHardwareEth = 1
	ARPOpRequest   = 1
	ARPOpReply     = 2
)

// Broadcast 广播 MAC。
var Broadcast = net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}

// ErrFrame 无效帧。
var ErrFrame = errors.New("vrndis/ethernet: malformed frame")

// Frame 解析后的以太网帧。
type Frame struct {
	Dst       net.HardwareAddr
	Src       net.HardwareAddr
	EtherType uint16
	Payload   []byte
}

// ParseFrame 解析以太网帧，帧长度不足返回 ErrFrame。
func ParseFrame(b []byte) (Frame, error) {
	if len(b) < HeaderLen {
		return Frame{}, fmt.Errorf("%w: %d bytes", ErrFrame, len(b))
	}
	f := Frame{
		Dst:       net.HardwareAddr(append([]byte(nil), b[0:6]...)),
		Src:       net.HardwareAddr(append([]byte(nil), b[6:12]...)),
		EtherType: binary.BigEndian.Uint16(b[12:14]),
		Payload:   b[14:],
	}
	return f, nil
}

// BuildFrame 构造以太网帧。
func BuildFrame(dst, src net.HardwareAddr, etherType uint16, payload []byte) []byte {
	frame := make([]byte, HeaderLen+len(payload))
	copy(frame[0:6], dst)
	copy(frame[6:12], src)
	binary.BigEndian.PutUint16(frame[12:14], etherType)
	copy(frame[14:], payload)
	return frame
}

// IsMulticast MAC 是否为组播/广播（首字节最低位）。
func IsMulticast(mac net.HardwareAddr) bool {
	return len(mac) > 0 && mac[0]&0x01 != 0
}

// MakeLocalMAC 生成本地管理单播 MAC：02:00:00:00:00:XX。
// index 0 → XX=0x00（保留给服务器网卡），1..255 → 0x01..0xFF（客户端），更高索引在 01-FF 内回绕。
func MakeLocalMAC(index uint64) net.HardwareAddr {
	var last byte
	if index == 0 {
		last = 0x00
	} else {
		last = byte(1 + (index-1)%255)
	}
	return net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, last}
}

// MACFromString 解析 "02:00:00:00:00:01" 形式的 MAC。
func MACFromString(s string) (net.HardwareAddr, error) {
	mac, err := net.ParseMAC(s)
	if err != nil || len(mac) != 6 {
		return nil, fmt.Errorf("%w: bad MAC %q", ErrFrame, s)
	}
	return mac, nil
}

// ARP ARP 报文（仅支持以太网/IPv4）。
type ARP struct {
	Op  uint16
	SHA net.HardwareAddr
	SPA net.IP
	THA net.HardwareAddr
	TPA net.IP
}

// ParseARP 解析 ARP 载荷（以太网帧去掉 14 字节头之后的部分）。
func ParseARP(payload []byte) (ARP, error) {
	if len(payload) < 28 {
		return ARP{}, fmt.Errorf("%w: ARP too short (%d)", ErrFrame, len(payload))
	}
	if binary.BigEndian.Uint16(payload[0:2]) != ARPHardwareEth ||
		binary.BigEndian.Uint16(payload[2:4]) != TypeIPv4 ||
		payload[4] != 6 || payload[5] != 4 {
		return ARP{}, fmt.Errorf("%w: unsupported ARP flavor", ErrFrame)
	}
	return ARP{
		Op:  binary.BigEndian.Uint16(payload[6:8]),
		SHA: net.HardwareAddr(append([]byte(nil), payload[8:14]...)),
		SPA: net.IP(append([]byte(nil), payload[14:18]...)),
		THA: net.HardwareAddr(append([]byte(nil), payload[18:24]...)),
		TPA: net.IP(append([]byte(nil), payload[24:28]...)),
	}, nil
}

// MarshalARP 编码 ARP 载荷。
func (a ARP) Marshal() []byte {
	b := make([]byte, 28)
	binary.BigEndian.PutUint16(b[0:2], ARPHardwareEth)
	binary.BigEndian.PutUint16(b[2:4], TypeIPv4)
	b[4], b[5] = 6, 4
	binary.BigEndian.PutUint16(b[6:8], a.Op)
	copy(b[8:14], a.SHA)
	copy(b[14:18], a.SPA.To4())
	copy(b[18:24], a.THA)
	copy(b[24:28], a.TPA.To4())
	return b
}

// ARPRequest 构造一条完整的 ARP 请求帧。
func ARPRequest(sha net.HardwareAddr, spa, tpa net.IP) []byte {
	return BuildFrame(Broadcast, sha, TypeARP,
		ARP{Op: ARPOpRequest, SHA: sha, SPA: spa, TPA: tpa}.Marshal())
}

// ARPReply 构造一条完整的 ARP 应答帧。
func ARPReply(sha net.HardwareAddr, spa net.IP, tha net.HardwareAddr, tpa net.IP) []byte {
	return BuildFrame(tha, sha, TypeARP,
		ARP{Op: ARPOpReply, SHA: sha, SPA: spa, THA: tha, TPA: tpa}.Marshal())
}
