package p2p

import (
	"crypto/rand"
	"encoding/binary"
	"net"
)

// random 用加密随机源填充 buf。
func random(buf []byte) (int, error) {
	return rand.Read(buf)
}

// decodeXORMapped 解码 STUN XOR-MAPPED-ADDRESS 属性（family=1 即 IPv4）。
// 编码格式（8 字节）：family(1) | 保留(1) | XOR端口(2) | XOR IPv4(4)。
func decodeXORMapped(raw []byte) (*net.UDPAddr, error) {
	if len(raw) < 8 || raw[0] != 1 {
		return nil, errNoXORAddr
	}
	key := []byte{0x21, 0x12, 0xA4, 0x42}
	xorPort := binary.BigEndian.Uint16(raw[1:3]) ^ binary.BigEndian.Uint16(key[0:2])
	var ip [4]byte
	for i := 0; i < 4; i++ {
		ip[i] = raw[4+i] ^ key[i%4]
	}
	return &net.UDPAddr{IP: net.IPv4(ip[0], ip[1], ip[2], ip[3]), Port: int(xorPort)}, nil
}
