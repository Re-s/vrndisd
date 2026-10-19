package server

import (
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// blockingConn 模拟对端半死的 TCP 连接：Write 无限阻塞（发送缓冲满 +
// 对端窗口关闭、无 RST）。SetWriteDeadline 后 Write 在截止时间返回
// （模拟真实 TCPConn 行为，writeMsg 依赖此机制封顶阻塞）。
type blockingConn struct {
	mu  sync.Mutex
	wdl time.Time
}

func (c *blockingConn) Read(b []byte) (int, error) { return 0, io.EOF }
func (c *blockingConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	dl := c.wdl
	c.mu.Unlock()
	var timer *time.Timer
	if dl.IsZero() {
		timer = time.NewTimer(time.Hour)
	} else {
		if d := time.Until(dl); d <= 0 {
			return 0, errors.New("i/o timeout")
		} else {
			timer = time.NewTimer(d)
		}
	}
	<-timer.C
	return 0, errors.New("i/o timeout")
}
func (c *blockingConn) Close() error                       { return nil }
func (c *blockingConn) LocalAddr() net.Addr                { return nil }
func (c *blockingConn) RemoteAddr() net.Addr               { return nil }
func (c *blockingConn) SetDeadline(t time.Time) error      { return c.setDL(t) }
func (c *blockingConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *blockingConn) SetWriteDeadline(t time.Time) error { return c.setDL(t) }
func (c *blockingConn) setDL(t time.Time) error {
	c.mu.Lock()
	c.wdl = t
	c.mu.Unlock()
	return nil
}

// TestWriteMsgTimeout - writeMsg 必须受 controlWriteTimeout 封顶：
// 对端半死、写无限阻塞时，10s 级超时内必须返回错误（修复前无限阻塞，
// writer 持 writeMu 冻结整个会话的处理循环）。
func TestWriteMsgTimeout(t *testing.T) {
	sess := &session{conn: &blockingConn{}}
	start := time.Now()
	err := sess.writeMsg(0x41, []byte("hello"))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("阻塞写应超时报错")
	}
	if elapsed > controlWriteTimeout+2*time.Second {
		t.Fatalf("写超时未生效: elapsed=%v", elapsed)
	}
	if elapsed < controlWriteTimeout-time.Second {
		t.Fatalf("写过早返回: elapsed=%v", elapsed)
	}
}

// TestSendControlNonBlocking - 收件人的写锁被（卡在半死对端的 writer）
// 长期占住时，sendControl 必须立即返回，不得冻结发件人。
func TestSendControlNonBlocking(t *testing.T) {
	sess := &session{conn: &blockingConn{}}
	sess.writeMu.Lock() // 模拟 writer 卡在半死对端、长期独占写锁
	defer sess.writeMu.Unlock()

	start := time.Now()
	done := make(chan struct{})
	go func() {
		sess.sendControl(0x43, []byte("push"))
		close(done)
	}()
	select {
	case <-done:
		if el := time.Since(start); el > 500*time.Millisecond {
			t.Fatalf("sendControl 被收件人写阻塞拖住: %v", el)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sendControl 未立即返回（发件人被冻结）")
	}
}
