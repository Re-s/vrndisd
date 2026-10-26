// Package loglevel 提供带级别的日志器与限速工具。
//
// 标准库 log.Logger 无级别概念，数据面逐帧事件（网卡队列满丢帧、
// 候选推送等）在弱网环境下会逐帧刷屏。Logger 嵌入 *log.Logger，
// 历史 Printf 调用点无需修改即可兼容，新代码逐步改用 Debugf 等级别方法。
// RateLimiter 用于抑制高频同类消息（如背压丢帧），同一 key 在
// 时间窗内只输出一次。
package loglevel

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// Level 日志级别。
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

// ParseLevel 解析级别字符串（不区分大小写）：debug/info/warn/error。
// 空字符串返回 LevelInfo；未知值返回错误。
func ParseLevel(s string) (Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return LevelInfo, nil
	case "debug":
		return LevelDebug, nil
	case "warn", "warning":
		return LevelWarn, nil
	case "error":
		return LevelError, nil
	}
	return LevelInfo, fmt.Errorf("未知日志级别 %q（可选 debug/info/warn/error）", s)
}

// String 级别名称。
func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "debug"
	case LevelInfo:
		return "info"
	case LevelWarn:
		return "warn"
	case LevelError:
		return "error"
	}
	return "info"
}

// Logger 带级别过滤的日志器。嵌入 *log.Logger 保留全部既有方法
// （Printf 即 Info 级别），已有调用点零改动平滑迁移。
type Logger struct {
	*log.Logger
	level Level
}

// Printf 覆盖嵌入的同名方法：历史调用点一律按 Info 级别过滤。
// 其余嵌入方法（Print/Fatal 等）保持原语义不过滤。
func (l *Logger) Printf(format string, v ...any) {
	if l.enabled(LevelInfo) {
		l.Logger.Printf(format, v...)
	}
}
func New(l *log.Logger, level Level) *Logger {
	return &Logger{Logger: l, level: level}
}

// Level 当前生效级别。
func (l *Logger) Level() Level {
	if l == nil {
		return LevelInfo
	}
	return l.level
}

// enabled 是否输出该级别（nil 安全）。
func (l *Logger) enabled(lv Level) bool {
	return l != nil && l.Logger != nil && l.level <= lv
}

// Debugf 据此排查细节（候选变化、逐帧背压等），默认不输出。
func (l *Logger) Debugf(format string, args ...any) {
	if l.enabled(LevelDebug) {
		l.Logger.Printf(format, args...)
	}
}

// Infof 常规事件（建连/重连/隧道生命周期），默认输出。
func (l *Logger) Infof(format string, args ...any) {
	if l.enabled(LevelInfo) {
		l.Logger.Printf(format, args...)
	}
}

// Warnf 可继续运行但值得注意（回退、降级、可选功能失败）。
func (l *Logger) Warnf(format string, args ...any) {
	if l.enabled(LevelWarn) {
		l.Logger.Printf(format, args...)
	}
}

// Errorf 需要介入的问题。
func (l *Logger) Errorf(format string, args ...any) {
	if l.enabled(LevelError) {
		l.Logger.Printf(format, args...)
	}
}

// RateLimiter 按 key 限速：同一 key 在 Window 内最多放行一次。
// 用于高频同类事件（背压丢帧）合并输出，避免逐帧刷屏。
// 零值不可用，需 NewRateLimiter 初始化。
type RateLimiter struct {
	mu     sync.Mutex
	last   map[string]time.Time
	window time.Duration
}

// NewRateLimiter 创建限速器；window <= 0 时每次都放行（禁用限速）。
func NewRateLimiter(window time.Duration) *RateLimiter {
	return &RateLimiter{
		last:   make(map[string]time.Time),
		window: window,
	}
}

// Allow 检查 key 是否允许输出。
func (r *RateLimiter) Allow(key string) bool {
	if r.window <= 0 {
		return true
	}
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.last[key]; ok && now.Sub(t) < r.window {
		return false
	}
	if len(r.last) > 256 { // 防止 key 无限增长
		r.last = make(map[string]time.Time)
	}
	r.last[key] = now
	return true
}
