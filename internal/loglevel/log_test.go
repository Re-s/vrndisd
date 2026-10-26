package loglevel

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"
)

func testLogger(level Level) (*Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return New(log.New(buf, "", 0), level), buf
}

func TestParseLevel(t *testing.T) {
	cases := map[string]Level{
		"":        LevelInfo,
		"info":    LevelInfo,
		"INFO":    LevelInfo,
		" debug ": LevelDebug,
		"warn":    LevelWarn,
		"warning": LevelWarn,
		"error":   LevelError,
	}
	for s, want := range cases {
		got, err := ParseLevel(s)
		if err != nil || got != want {
			t.Errorf("ParseLevel(%q) = %v, %v; want %v, nil", s, got, err, want)
		}
	}
	if _, err := ParseLevel("trace"); err == nil {
		t.Error("ParseLevel(trace) 应返回错误")
	}
}

func TestLevelFiltering(t *testing.T) {
	l, buf := testLogger(LevelInfo)
	l.Debugf("hidden %s", "detail")
	if buf.Len() > 0 {
		t.Errorf("info 级别不应输出 debug: %q", buf.String())
	}
	l.Infof("info-msg")
	l.Warnf("warn-msg")
	l.Errorf("error-msg")
	out := buf.String()
	for _, want := range []string{"info-msg", "warn-msg", "error-msg"} {
		if !strings.Contains(out, want) {
			t.Errorf("缺少 %q，输出: %q", want, out)
		}
	}

	l, buf = testLogger(LevelWarn)
	l.Infof("hidden-info")
	if buf.Len() > 0 {
		t.Errorf("warn 级别不应输出 info: %q", buf.String())
	}
	l.Warnf("warn-ok")
	if !strings.Contains(buf.String(), "warn-ok") {
		t.Error("warn 级别应输出 warn")
	}

	// Printf 是历史兼容入口（等同 Info），任何级别配置下行为不变。
	l, buf = testLogger(LevelInfo)
	l.Printf("legacy")
	if !strings.Contains(buf.String(), "legacy") {
		t.Error("Printf(info 级别) 应输出")
	}
	// Printf=Info：error 级别下同样被过滤（分级语义的一致性）。
	l, buf = testLogger(LevelError)
	l.Printf("legacy-suppressed")
	if buf.Len() > 0 {
		t.Errorf("error 级别不应输出 Printf: %q", buf.String())
	}
}

func TestLoggerNilSafe(t *testing.T) {
	// 嵌入的标准库日志器为 nil 时不应 panic（防御性：测试与嵌入用法）。
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil Logger 不应 panic: %v", r)
		}
	}()
	l := New(nil, LevelDebug)
	l.Debugf("nothing")
	l.Infof("nothing")
	l.Warnf("nothing")
	l.Errorf("nothing")
	if l.Level() != LevelDebug {
		t.Errorf("nil Logger Level() = %v, want debug", l.Level())
	}
}

func TestRateLimiter(t *testing.T) {
	r := NewRateLimiter(50 * time.Millisecond)
	if !r.Allow("k") {
		t.Error("首次应放行")
	}
	if r.Allow("k") {
		t.Error("时间窗内重复 key 应拒绝")
	}
	if !r.Allow("other") {
		t.Error("不同 key 互不影响")
	}
	time.Sleep(60 * time.Millisecond)
	if !r.Allow("k") {
		t.Error("时间窗过后应放行")
	}

	// window <= 0 禁用限速。
	r = NewRateLimiter(0)
	if !r.Allow("k") || !r.Allow("k") {
		t.Error("window<=0 应总是放行")
	}
}
