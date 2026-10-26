package client

import (
	"bytes"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"vrndis/internal/proto"
	"vrndis/internal/tap"
)

// runSockToDev 向连接灌入 frames 帧数据（网卡队列满后逐帧触发背压），
// 返回客户端与捕获的日志输出。
func runSockToDev(t *testing.T, frames int, logCfg string) (*Client, string) {
	t.Helper()

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	c := New(Config{
		Server:      "127.0.0.1:1",
		TapName:     "c0",
		DialTimeout: time.Second,
		Device:      tap.NewFake("c0"), // toApp 队列满后 Write 返回 ErrBufferFull
		LogLevel:    logCfg,
	})
	// sockToDev 经 connectOnce 才会赋值 c.dev；本测试直接驱动读方向，手动注入。
	if c.dev == nil {
		c.dev = tap.NewFake("c0")
	}

	local, remote := net.Pipe()
	done := make(chan struct{})
	c.connWG.Add(1)

	go func() {
		// 读取客户端回写（本测试无回写，仅耗尽避免管道阻塞）。
		io.Copy(io.Discard, local)
	}()
	go func() {
		for i := 0; i < frames; i++ {
			if err := proto.WriteMessage(local, proto.TypeData, []byte("payload")); err != nil {
				return
			}
		}
		local.Close() // 写完关闭 → 读端 EOF，sockToDev 正常退出
	}()

	c.sockToDev(remote, done)
	return c, buf.String()
}

func TestSockToDevDropFramesRateLimited(t *testing.T) {
	// 1000 帧写入 256 容量的队列：背压逐帧触发，但日志每秒最多一条。
	const frames = 1000
	_, out := runSockToDev(t, frames, "debug")

	lines := strings.Split(strings.TrimSpace(out), "\n")
	dropLines := 0
	for _, ln := range lines {
		if strings.Contains(ln, "网卡队列满") {
			dropLines++
		}
	}
	if dropLines == 0 {
		t.Fatalf("debug 级别应至少输出一条背压日志: %q", out)
	}
	if dropLines > 2 {
		t.Errorf("背压日志应限速（1000 帧在毫秒内完成），实际 %d 条:\n%s", dropLines, out)
	}
	t.Logf("1000 帧背压合并为 %d 条日志", dropLines)
}

func TestSockToDevDropFramesSuppressedAtInfo(t *testing.T) {
	_, out := runSockToDev(t, 500, "")
	if strings.Contains(out, "网卡队列满") {
		t.Errorf("默认 info 级别不应输出逐帧背压日志: %q", out)
	}
}

func TestNewParsesLogLevel(t *testing.T) {
	for _, tc := range []struct {
		cfg  string
		want int // loglevel.Level 值
	}{
		{"", 1}, {"info", 1}, {"debug", 0}, {"warn", 2}, {"error", 3},
		{"bogus", 1}, // 非法值回退 info
	} {
		c := New(Config{Server: "127.0.0.1:1", LogLevel: tc.cfg})
		if got := int(c.logger.Level()); got != tc.want {
			t.Errorf("LogLevel %q → 级别 %d, want %d", tc.cfg, got, tc.want)
		}
	}
}
