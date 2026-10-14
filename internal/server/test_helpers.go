package server

import (
	"fmt"
	"math/rand"
	"strings"
	"time"
)

var testRand *rand.Rand

func init() {
	testRand = rand.New(rand.NewSource(time.Now().UnixNano()))
}

// NewTestNetwork 生成一个测试用的网段
func NewTestNetwork() (string, string) {
	// 使用 10.128.0.0/24 ~ 10.255.0.0/24 范围，避免与常见网段冲突
	secondOctet := uint8(128 + testRand.Intn(128))
	thirdOctet := uint8(testRand.Intn(256))
	network := fmt.Sprintf("10.%d.%d.0/24", secondOctet, thirdOctet)
	gateway := fmt.Sprintf("10.%d.%d.1", secondOctet, thirdOctet)
	return network, gateway
}

// IPInRange 检查 IP 是否在网段内（简单检查）
func IPInRange(ip, network string) bool {
	ipParts := strings.Split(ip, ".")
	netParts := strings.Split(strings.Split(network, "/")[0], ".")
	if len(ipParts) != len(netParts) {
		return false
	}
	for i := range ipParts {
		if ipParts[i] != netParts[i] {
			return false
		}
	}
	return true
}
