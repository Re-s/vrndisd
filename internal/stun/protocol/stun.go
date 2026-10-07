package protocol

import (
	"encoding/binary"
	"fmt"
)

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
	Type        MsgType
	Length      uint16
	MagicCookie uint32
	TransactionID [12]byte
	Attributes  map[AttrType][]byte
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
