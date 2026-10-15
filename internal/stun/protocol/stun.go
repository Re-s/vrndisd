package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
)

// ErrBadXORAddr XOR-MAPPED-ADDRESS 属性缺失或不是 IPv4 家族。
var ErrBadXORAddr = errors.New("vrndis/proto: bad XOR-MAPPED-ADDRESS")

type MsgType uint16
type AttrType uint16

const (
	BindingRequest  MsgType = 0x0001
	BindingResponse MsgType = 0x0101
	BindingError    MsgType = 0x0102
)
const AttrXORMappedAddr AttrType = 0x0020
const MagicCookie = 0x2112A442

type Message struct {
	Type          MsgType
	Length        uint16
	MagicCookie   uint32
	TransactionID [12]byte
	Attributes    map[AttrType][]byte
}

func Parse(data []byte) (*Message, error) {
	if len(data) < 20 {
		return nil, fmt.Errorf("short")
	}
	m := &Message{Attributes: make(map[AttrType][]byte)}
	m.Type = MsgType(binary.BigEndian.Uint16(data[0:2]))
	m.Length = binary.BigEndian.Uint16(data[2:4])
	m.MagicCookie = uint32(binary.BigEndian.Uint32(data[4:8]))
	copy(m.TransactionID[:], data[8:20])
	// Parse the attribute TLV section (RFC 5389 §15).
	// Attribute section starts at byte 20; its length is bounded by the
	// buffer length, not the (sometimes unreliable) Length header field.
	off := 20
	for off+4 <= len(data) {
		attrType := AttrType(binary.BigEndian.Uint16(data[off : off+2]))
		attrLen := int(binary.BigEndian.Uint16(data[off+2 : off+4]))
		off += 4
		if off+attrLen > len(data) {
			// Malformed / truncated attribute: stop rather than panic.
			break
		}
		val := make([]byte, attrLen)
		copy(val, data[off:off+attrLen])
		m.Attributes[attrType] = val
		off += attrLen
		// STUN attributes are padded to a 4-byte boundary.
		if pad := attrLen % 4; pad != 0 {
			off += 4 - pad
		}
	}
	return m, nil
}

func TypeName(t MsgType) string {
	switch t {
	case BindingRequest:
		return "BIND_REQUEST"
	case BindingResponse:
		return "BIND_RESPONSE"
	case BindingError:
		return "BIND_ERROR"
	default:
		return "?UNKNOWN"
	}
}

// DecodeXORMapped 解码 STUN XOR-MAPPED-ADDRESS 属性（family=1 即 IPv4）。
// 编码格式（8 字节）：family(1) | 保留(1) | XOR端口(2) | XOR IPv4(4)。
func DecodeXORMapped(raw []byte) (*net.UDPAddr, error) {
	if len(raw) < 8 || raw[0] != 1 {
		return nil, ErrBadXORAddr
	}
	key := []byte{0x21, 0x12, 0xA4, 0x42}
	xorPort := binary.BigEndian.Uint16(raw[1:3]) ^ binary.BigEndian.Uint16(key[0:2])
	var ip [4]byte
	for i := 0; i < 4; i++ {
		ip[i] = raw[4+i] ^ key[i%4]
	}
	return &net.UDPAddr{IP: net.IPv4(ip[0], ip[1], ip[2], ip[3]), Port: int(xorPort)}, nil
}
