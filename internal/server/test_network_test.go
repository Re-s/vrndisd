// 测试辅助：随机网段生成，避免与运行中的服务（10.77.0.0/24）冲突。
package server_test

import (
	"fmt"
	"math/rand"
	"net"
	"time"
)

// randTestNetwork 生成随机 /24 测试网段。
// 返回 (network CIDR, gateway IP)。
func randTestNetwork() (network, gateway string) {
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	b := 100 + r.Intn(100) // 10.100~199.x
	c := r.Intn(256)
	network = fmt.Sprintf("10.%d.%d.0/24", b, c)
	gateway = fmt.Sprintf("10.%d.%d.1", b, c)
	return
}

// randTestNetwork30 生成随机 /30 网段（仅 1 个可用客户端地址 .2）。
// 返回 (network CIDR, gateway IP, client IP)。
func randTestNetwork30() (network, gateway, clientIP string) {
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	b := 100 + r.Intn(100)
	c := r.Intn(256)
	network = fmt.Sprintf("10.%d.%d.0/30", b, c)
	gateway = fmt.Sprintf("10.%d.%d.1", b, c)
	clientIP = fmt.Sprintf("10.%d.%d.2", b, c)
	return
}

// firstClientIP 从网关地址推算第一个客户端 IP（.1 → .2）。
func firstClientIP(gateway string) string {
	gw := net.ParseIP(gateway).To4()
	if gw == nil {
		return ""
	}
	gw[3]++
	return gw.String()
}
