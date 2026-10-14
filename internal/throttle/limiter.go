// Package throttle 实现基于令牌桶的限速器。
//
// 用于限制单个客户端的入站帧速率，防止单个高流量客户端
// 占满服务器资源（令牌桶兼顾突发与平均速率）。
package throttle

import (
	"sync"
	"time"
)

// Limiter 令牌桶限速器。
//   - rate:     每秒补充的令牌数（允许的稳态速率）
//   - burst:    桶容量（允许的瞬时突发量）
type Limiter struct {
	mu       sync.Mutex
	rate     float64       // 令牌/秒
	burst    float64       // 桶容量
	tokens   float64       // 当前令牌数
	lastTime time.Time     // 上次补充时间
}

// New 创建限速器。rate 为每秒允许通过的数量，burst 为突发容量。
// rate <= 0 表示不限速（Allow 永远返回 true）。
func New(rate int, burst int) *Limiter {
	return &Limiter{
		rate:     float64(rate),
		burst:    float64(burst),
		tokens:   float64(burst), // 初始满桶
		lastTime: time.Now(),
	}
}

// Allow 尝试消费一个令牌。成功返回 true，超限返回 false。
func (l *Limiter) Allow() bool {
	if l.rate <= 0 {
		return true // 未启用限速
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	// 按时间差补充令牌
	now := time.Now()
	elapsed := now.Sub(l.lastTime).Seconds()
	l.lastTime = now
	l.tokens += elapsed * l.rate
	if l.tokens > l.burst {
		l.tokens = l.burst // 不超过桶容量
	}

	if l.tokens >= 1 {
		l.tokens--
		return true
	}
	return false
}

// Tokens 返回当前可用令牌数（浮点，调试/监控用）。
func (l *Limiter) Tokens() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.tokens
}
