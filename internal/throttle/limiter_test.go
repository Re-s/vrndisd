package throttle

import (
	"testing"
	"time"
)

// 未启用限速（rate=0）时 Allow 永远返回 true
func TestLimiterDisabled(t *testing.T) {
	l := New(0, 10)
	for i := 0; i < 1000; i++ {
		if !l.Allow() {
			t.Fatal("rate=0 时不应限速")
		}
	}
}

// 突发容量内全部放行
func TestLimiterBurst(t *testing.T) {
	l := New(100, 50) // 100/s，桶 50
	for i := 0; i < 50; i++ {
		if !l.Allow() {
			t.Fatalf("突发容量内第 %d 个帧应放行", i)
		}
	}
}

// 超出突发容量后被限流
func TestLimiterExceedBurst(t *testing.T) {
	l := New(10, 5) // 10/s，桶 5
	for i := 0; i < 5; i++ {
		l.Allow()
	}
	if l.Allow() {
		t.Fatal("超出突发容量后应限流")
	}
}

// 令牌按时间补充：等待后恢复放行
func TestLimiterRefillOverTime(t *testing.T) {
	l := New(100, 2) // 100/s，桶 2
	l.Allow()
	l.Allow()
	if l.Allow() {
		t.Fatal("桶空时应限流")
	}
	// 100 token/s => 10ms 补充 1 个
	time.Sleep(30 * time.Millisecond)
	if !l.Allow() {
		t.Fatal("等待补充后应恢复放行")
	}
}

// 并发安全：高频并发调用不 panic
func TestLimiterConcurrent(t *testing.T) {
	l := New(1000, 100)
	done := make(chan struct{}, 10)
	for i := 0; i < 10; i++ {
		go func() {
			for j := 0; j < 100; j++ {
				l.Allow()
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < 10; i++ {
		<-done
	}
}
