// Package protocol 提供 STUN 协议实现
package protocol

import (
	"encoding/binary"
	"fmt"
	"net"
)

// STUN 消息类型
const (
	TypeBindingRequest  = 0x0001
	TypeBindingResponse = 0x0101
	TypeBindingError    = 0x0102
)

// Attribute types
const (
	AttrMappedAddress     = 0x0001
	Attr.Username         = 0x0006
	Attr.MessageIntegrity = 0x0008
	Attr.Experimental     = 0x8001
	Attr.NATType          = 0x802c
	Attr.XOR_MAPPED_ADDR  = 0x0020
)

// STUN Message header: Magic Cookie + Transaction ID (16 bytes total)
type Message struct {
	Type       uint16
	Length     uint16
	MagicCookie uint32
	TransactionID [12]byte
	Attributes  map[uint16][]byte // simple storage for attributes
}

const (
	BaseHeaderSize = 20 // 20-byte STUN header
	// Transaction ID is 12 bytes, magic cookie is 4 bytes
	TransactionIDLen = 12
)

var MagicCookie = [4]byte{0x21, 0x12, 0xA4, 0x42}

// ParseMessage parses a STUN message from the given bytes
func ParseMessage(data []byte) (*Message, error) {
	if len(data) < 20 {
		return nil, fmt.Errorf("STUN message too short")
	}

	msg := &Message{
		Attributes: make(map[uint16][]byte),
	}
	msg.Type = binary.BigEndian.Uint16(data[0:2])
	msg.Length = binary.BigEndian.Uint16(data[2:4])
	msg.MagicCookie = binary.BigEndian.Uint32(data[4:8])
	copy(msg.TransactionID[:], data[8:20])

	// Parse attributes
	offset := 20
	for offset < len(data) {
		if offset+4 > len(data) {
			break
		}
		attrType := binary.BigEndian.Uint16(data[offset : offset+2])
		attrLen := binary.BigEndian.Uint16(data[offset+2 : offset+4])
		if offset+4+int(attrLen) > len(data) {
			// Padding, ignore
			break
		}
		msg.Attributes[attrType] = make([]byte, attrLen)
		copy(msg.Attributes[attrType], data[offset+4:offset+4+int(attrLen)])
		offset += 4 + int(attrLen)
		// Align to 4 bytes
		remainder := offset % 4
		if remainder != 0 {
			offset += 4 - remainder
		}
	}

	return msg, nil
}

// BuildMessage creates a STUN message with the given type and attributes
func BuildMessage(msgType uint16, attributes map[uint16][]byte) ([]byte, error) {
	// Calculate length
	data := make([]byte, 0, 20+32)
	data = append(data, []byte{
		byte(msgType >> 8),
		byte(msgType),
		0, 0, // length will be set later
		byte(MagicCookie[0]),
		byte(MagicCookie[1]),
		byte(MagicCookie[2]),
		byte(MagicCookie[3]),
	}...)

	// Transaction ID - random
	// In real implementation, this should be unique
	for i := 0; i < 12; i++ {
		data = append(data, 0)
	}
	msgLength := 0

	// Build attributes
	for attrType, attrValue := range attributes {
		data = append(data, []byte{
			byte(attrType >> 8),
			byte(attrType),
			byte(len(attrValue) >> 8),
			byte(len(attrValue)),
		}...)
		data = append(data, attrValue...)
		// Pad to 4-byte boundary
		if len(attrValue)%4 != 0 {
			padding := 4 - (len(attrValue) % 4)
			paddingBytes := make([]byte, padding)
			data = append(data, paddingBytes...)
		}
		msgLength += 4 + len(attrValue)
		if len(attrValue)%4 != 0 {
			msgLength += 4 - (len(attrValue) % 4)
		}
	}

	// Set length
	data[2] = byte(msgLength >> 8)
	data[3] = byte(msgLength)

	return data, nil
}

// MappedAddress returns the mapped address from a STUN message
func (m *Message) MappedAddress() (net.IP, int, error) {
	addrData, ok := m.Attributes[AttrXORMappedAddress]
	if !ok {
		return nil, 0, fmt.Errorf("no XOR_MAPPED_ADDRESS attribute")
	}
	if len(addrData) < 8 {
		return nil, 0, fmt.Errorf("XOR_MAPPED_ADDRESS too short")
	}
	// First two bytes are family and port
	family := addrData[0]
	port := binary.BigEndian.Uint16(addrData[1:3])
	ip := addrData[4:8]
	if family != 0x01 {
		return nil, 0, fmt.Errorf("unsupported address family: %d", family)
	}
	return ip, int(port), nil
}
