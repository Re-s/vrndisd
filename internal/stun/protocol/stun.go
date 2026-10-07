// Package protocol 提供 STUN 协议实现
package protocol

import (
	"encoding/binary"
	"fmt"
	"net"
)

// STUN Message Types
const (
	TypeBindingRequest  = 0x0001
	TypeBindingResponse = 0x0101
	TypeBindingError    = 0x0102
)

// Attribute Types
const (
	AttrMappedAddress     = 0x0001
	AttrUsername          = 0x0006
	AttrMessageIntegrity  = 0x0008
	AttrExperimental      = 0x8001
	AttrNATType           = 0x802c
	AttrXORMappedAddress  = 0x0020
)

// STUN Message structure
type Message struct {
	Type       uint16
	Length     uint16
	MagicCookie uint32
	TransactionID [12]byte
	Attributes  map[uint16][]byte
}

const (
	BaseHeaderSize = 20
	TransactionIDLen = 12
)

var MagicCookie = [4]byte{0x21, 0x12, 0xA4, 0x42}

// ParseMessage parses a STUN message
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

	offset := 20
	for offset < len(data) {
		if offset+4 > len(data) {
			break
		}
		attrType := binary.BigEndian.Uint16(data[offset : offset+2])
		attrLen := binary.BigEndian.Uint16(data[offset+2 : offset+4])
		if offset+4+int(attrLen) > len(data) {
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

// BuildMessage creates a STUN message
func BuildMessage(msgType uint16, attributes map[uint16][]byte) ([]byte, error) {
	data := make([]byte, 0, 20+32)
	data = append(data, []byte{
		byte(msgType >> 8),
		byte(msgType),
		0, 0,
		byte(MagicCookie[0]),
		byte(MagicCookie[1]),
		byte(MagicCookie[2]),
		byte(MagicCookie[3]),
	}...)
	for i := 0; i < 12; i++ {
		data = append(data, 0)
	}
	msgLength := 0

	for attrType, attrValue := range attributes {
		data = append(data, []byte{
			byte(attrType >> 8),
			byte(attrType),
			byte(len(attrValue) >> 8),
			byte(len(attrValue)),
		}...)
		data = append(data, attrValue...)
		if len(attrValue)%4 != 0 {
			padding := 4 - (len(attrValue) % 4)
			data = append(data, make([]byte, padding)...)
		}
		msgLength += 4 + len(attrValue)
		if len(attrValue)%4 != 0 {
			msgLength += 4 - (len(attrValue) % 4)
		}
	}
	data[2] = byte(msgLength >> 8)
	data[3] = byte(msgLength)
	return data, nil
}

// TypeName returns message name
func TypeName(t uint16) string {
	switch t {
	case TypeBindingRequest:
		return "BIND_REQUEST"
	case TypeBindingResponse:
		return "BIND_RESPONSE"
	case TypeBindingError:
		return "BIND_ERROR"
	default:
		return fmt.Sprintf("TYPE_%x", t)
	}
}
