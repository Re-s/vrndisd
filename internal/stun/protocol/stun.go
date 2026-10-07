package protocol

import (
	"encoding/binary"
	"fmt"
)

const (
	TypeBindingRequest  uint16 = 0x0001
	TypeBindingResponse uint16 = 0x0101
	TypeBindingError    uint16 = 0x0102
)

const AttrXORMappedAddress uint16 = 0x0020

type Message struct {
	Type        uint16
	Length      uint16
	MagicCookie uint32
	TransactionID [12]byte
	Attributes  map[uint16][]byte
}

var MagicCookie = [4]byte{0x21, 0x12, 0xA4, 0x42}

func Parse(data []byte) (*Message, error) {
	if len(data) < 20 {
		return nil, fmt.Errorf("too short")
	}
	m := &Message{Attributes: make(map[uint16][]byte)}
	m.Type = binary.BigEndian.Uint16(data[0:2])
	m.Length = binary.BigEndian.Uint16(data[2:4])
	m.MagicCookie = binary.BigEndian.Uint32(data[4:8])
	copy(m.TransactionID[:], data[8:20])
	offset := 20
	for offset+4 <= len(data) && offset+4+int(attrLen) <= len(data) {
		attrType := binary.BigEndian.Uint16(data[offset : offset+2])
		attrLen := binary.BigEndian.Uint16(data[offset+2 : offset+4])
		buf := make([]byte, attrLen)
		copy(buf, data[offset+4:offset+4+int(attrLen)])
		m.Attributes[attrType] = buf
		offset += 4 + int(attrLen)
		rem := offset % 4
		if rem != 0 {
			offset += 4 - rem
		}
	}
	return m, nil
}

func TypeName(t uint16) string {
	switch t {
	case TypeBindingRequest:
		return "BIND_REQUEST"
	case TypeBindingResponse:
		return "BIND_RESPONSE"
	case TypeBindingError:
		return "BIND_ERROR"
	default:
		return "UNKNOWN"
	}
}
