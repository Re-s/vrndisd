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
	"time"

	"github.com/Re-s/vrndisd/internal/ethernet"
)

const (
	DefaultMaxEntries   = 4096              // 最大 MAC 表条目数
	DefaultAgingTime    = 300 * time.Second // 条目老化时间
	DefaultCleanupRatio = 0.1               // 每次清理淘汰 10% 的旧条目
)

// MacEntry 一个 MAC 表条目，包含端口和访问时间
type MacEntry struct {
	Port       string
	AccessedAt time.Time
}

// LearningSwitch 支持上限和老化的学习型交换机。
type LearningSwitch struct {
	mu    sync.Mutex
	table map[string]*MacEntry // MAC 字符串 -> 条目
	max   int                  // 最大条目数
	aging time.Duration        // 老化时间
	clean bool                 // 是否启用自动清理
}

// New 创建空交换机，使用默认配置。
func New() *LearningSwitch {
	return NewWithConfig(4096, 300*time.Second, true)
}

// NewWithConfig 创建指定配置的交换机。
func NewWithConfig(max int, aging time.Duration, autoCleanup bool) *LearningSwitch {
	return &LearningSwitch{
		table: make(map[string]*MacEntry),
		max:   max,
		aging: aging,
		clean: autoCleanup,
	}
}

// Learn 记录源 MAC 从哪个端口进来。
func (s *LearningSwitch) Learn(mac net.HardwareAddr, port string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	macStr := mac.String()
	now := time.Now()

	// 如果 MAC 已存在，更新端口和时间
	if entry, ok := s.table[macStr]; ok {
		entry.Port = port
		entry.AccessedAt = now
		return
	}

	// 检查是否需要淘汰旧条目
	if len(s.table) >= s.max {
		s.evictOldest()
	}

	// 添加新条目
	s.table[macStr] = &MacEntry{
		Port:       port,
		AccessedAt: now,
	}
}

// evictOldest 淘汰最旧的条目（LRU）
func (s *LearningSwitch) evictOldest() {
	s.cleanOld(false) // 清理后只占满阈值
	// 如果仍然满，则强制清理更多
	for len(s.table) >= s.max && len(s.table) > 0 {
		s.evictOnce()
	}
}

// evictOnce 强制淘汰一个最旧的条目
func (s *LearningSwitch) evictOnce() {
	var oldestTitle string
	oldestTime := time.Now()
	for mac, entry := range s.table {
		if entry.AccessedAt.Before(oldestTime) {
			oldestTime = entry.AccessedAt
			oldestTitle = mac
		}
	}
	if oldestTitle != "" {
		delete(s.table, oldestTitle)
	}
}

// cleanOld 清理所有已过期的条目
func (s *LearningSwitch) cleanOld(continueToMax bool) {
	now := time.Now()
	toDelete := []string{}

	// 标记所有过期条目
	for mac, entry := range s.table {
		if now.Sub(entry.AccessedAt) > s.aging {
			toDelete = append(toDelete, mac)
		}
	}

	// 删除过期条目
	for _, mac := range toDelete {
		delete(s.table, mac)
	}

	// 如果需要继续清理到最大值，则继续
	if continueToMax && len(s.table) >= s.max {
		// 清理百分比
		cleanCount := int(float64(len(s.table)) * DefaultCleanupRatio)
		if cleanCount > 0 {
			for i := 0; i < cleanCount && len(s.table) > 0; i++ {
				s.evictOnce()
			}
		}
	}
}

// Lookup 查 MAC 表，返回端口和是否找到。
func (s *LearningSwitch) Lookup(mac net.HardwareAddr) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 尝试清理过期（如果启用自动清理）
	if s.clean {
		s.cleanOld(false)
	}

	p, ok := s.table[mac.String()]
	if !ok {
		return "", false
	}
	return p.Port, ok
}

// Forget 清除某端口的全部 MAC 学习记录（端口下线时调用）。
func (s *LearningSwitch) Forget(port string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for mac, entry := range s.table {
		if entry.Port == port {
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
	// 尝试清理过期
	if s.clean {
		s.cleanOld(false)
	}
	known, ok := s.table[dst.String()]
	s.mu.Unlock()

	if !ok {
		return out // 未知单播：泛洪
	}
	if known == nil { // 已不会出现
		return out
	}
	if known.Port == ingress {
		// 目的地址在入端口自身：直接丢弃（不需要转发回本网卡）
		return map[string]bool{}
	}
	return map[string]bool{known.Port: true}
}

// Size 当前 MAC 表条目数（不含过期条目）。
func (s *LearningSwitch) Size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.table)
}

// MaxSize 返回最大容量。
func (s *LearningSwitch) MaxSize() int {
	return s.max
}

// Cleanup 手动触发一次过期清理。
func (s *LearningSwitch) Cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanOld(false)
}

// Stats 返回当前交换机统计信息。
func (s *LearningSwitch) Stats() (total, used, max int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 统计过期
	expired := 0
	now := time.Now()
	for _, entry := range s.table {
		if now.Sub(entry.AccessedAt) > s.aging {
			expired++
		}
	}
	return len(s.table), len(s.table) - expired, s.max
}
