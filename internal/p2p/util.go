package p2p

import (
	"crypto/rand"
	"net"

	"github.com/Re-s/vrndisd/internal/stun/protocol"
)

// random 用加密随机源填充 buf。
func random(buf []byte) (int, error) {
	return rand.Read(buf)
}

// decodeXORMapped 保留薄包装，实际实现在 internal/stun/protocol（共享）。
func decodeXORMapped(raw []byte) (*net.UDPAddr, error) {
	return protocol.DecodeXORMapped(raw)
}
