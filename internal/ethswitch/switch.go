// Package ethswitch 实现学习型二层交换机。
//
// 端口用字符串标识（"tap" 表示服务器内核端口，MAC 字符串表示客户端端口）：
//   - 组播/广播 → 泛洪到除入端口外的所有端口
//   - 已知单播   → 发往对应端口
//   - 未知单播   → 泛洪
package ethswitch

import (
	"net"
	"sync"

	"vrndis/internal/ethernet"
)

// LearningSwitch MAC 学习交换机。
type LearningSwitch struct {
	mu    sync.Mutex
	table map[string]string // MAC 字符串 -> 端口
}

// New 创建空交换机。
func New() *LearningSwitch {
	return &LearningSwitch{table: make(map[string]string)}
}

// Learn 记录源 MAC 从哪个端口进来。
func (s *LearningSwitch) Learn(mac net.HardwareAddr, port string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.table[mac.String()] = port
}

// Lookup 查 MAC 表。
func (s *LearningSwitch) Lookup(mac net.HardwareAddr) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.table[mac.String()]
	return p, ok
}

// Forget 清除某端口的全部 MAC 学习记录（端口下线时调用）。
func (s *LearningSwitch) Forget(port string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for mac, p := range s.table {
		if p == port {
			delete(s.table, mac)
		}
	}
}

// Forward 计算转发端口集合（不含入端口）。
func (s *LearningSwitch) Forward(dst net.HardwareAddr, ingress string, all map[string]bool) map[string]bool {
	out := make(map[string]bool, len(all))
	for port := range all {
		if port != ingress {
			out[port] = true
		}
	}
	if ethernet.IsMulticast(dst) {
		return out
	}
	s.mu.Lock()
	known, ok := s.table[dst.String()]
	s.mu.Unlock()
	if !ok {
		return out // 未知单播：泛洪
	}
	if known == ingress {
		return map[string]bool{}
	}
	return map[string]bool{known: true}
}

// Size 当前 MAC 表条目数。
func (s *LearningSwitch) Size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.table)
}
