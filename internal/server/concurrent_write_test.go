package server

import (
	"bytes"
	"fmt"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"

	"vrndis/internal/proto"
)

// fakeMuxConn 模拟真实 TCP socket 的关键行为：单次 Write 原子，
// 但不同 goroutine 的两次 Write 之间（系统调用边界）可以交错。
// Write 末尾的 Gosched 强制让出，制造 hdr 与 body 分开写的时序，
// 使 writeMsg 不持锁时的字节流交错可被稳定复现。
type fakeMuxConn struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	gosched bool
}

func (f *fakeMuxConn) Write(p []byte) (int, error) {
	f.mu.Lock()
	n, err := f.buf.Write(p)
	f.mu.Unlock()
	if err != nil {
		return n, err
	}
	// 在系统调用边界（Write 返回后、调用方发起下一次 Write 前）让出，
	// 制造 hdr 与 body 之间被其他 goroutine 整条消息插入的机会。
	if f.gosched {
		runtime.Gosched()
	}
	return n, nil
}

// Len / Snapshot 在外部锁内读取底层缓冲（供消费端断言）。
func (f *fakeMuxConn) Len() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buf.Len()
}
func (f *fakeMuxConn) Snapshot() *bytes.Buffer {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := bytes.NewBuffer(nil)
	cp.Write(f.buf.Bytes())
	return cp
}
func (f *fakeMuxConn) Read(p []byte) (int, error)         { return 0, nil }
func (f *fakeMuxConn) Close() error                       { return nil }
func (f *fakeMuxConn) LocalAddr() net.Addr                { return nil }
func (f *fakeMuxConn) RemoteAddr() net.Addr               { return nil }
func (f *fakeMuxConn) SetDeadline(t time.Time) error      { return nil }
func (f *fakeMuxConn) SetReadDeadline(t time.Time) error  { return nil }
func (f *fakeMuxConn) SetWriteDeadline(t time.Time) error { return nil }

// TestWriterConcurrentNoInterleave - 数据帧 writer 与控制消息 send 并发写
// 同一连接时，字节流不得交错（writeMsg 的 5 字节头与 body 分两次写）。
// writeMu 缺失时 hdr/body 交错、对端解出乱序 seq ->
// "replayed message" 误断连（高负载下必发）。
func TestWriterConcurrentNoInterleave(t *testing.T) {
	const controlMsgs = 40
	const dataFrames = 80

	body := bytes.Repeat([]byte{0xAB}, 900) // 大 body，交错时立即解析错位
	fake := &fakeMuxConn{gosched: true}
	sess := &session{
		conn: fake,
		outQ: make(chan []byte, controlMsgs+dataFrames),
		done: make(chan struct{}),
	}
	go sess.writer()
	t.Cleanup(func() { close(sess.done) })

	var wg sync.WaitGroup
	for i := 0; i < controlMsgs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = sess.send(proto.TypePing, body)
		}()
	}
	for i := 0; i < dataFrames; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sess.enqueueFrame(body)
		}()
	}
	wg.Wait()

	// 等 writer 把队列排空：buf 长度稳定即收尾
	deadline := time.Now().Add(5 * time.Second)
	var last int
	for time.Now().Before(deadline) {
		now := fake.Len()
		if now == last && len(sess.outQ) == 0 {
			break
		}
		last = now
		time.Sleep(20 * time.Millisecond)
	}

	// 消费端断言：全部消息必须完整可解析，少一条或长度不符即交错
	msgs, err := parseAll(fake.Snapshot())
	if err != nil {
		t.Fatalf("消息流解析失败（字节流交错）: %v", err)
	}
	if want := controlMsgs + dataFrames; len(msgs) != want {
		t.Fatalf("消息数不符: got %d want %d（字节流交错或帧丢失）", len(msgs), want)
	}
	for i, m := range msgs {
		if len(m.Body) != len(body) {
			t.Fatalf("第 %d 条消息 body 长度 %d != %d（hdr/body 交错）", i, len(m.Body), len(body))
		}
	}
}

func parseAll(buf *bytes.Buffer) ([]proto.Message, error) {
	var msgs []proto.Message
	for buf.Len() >= 5 {
		m, err := proto.ReadMessage(buf)
		if err != nil {
			return nil, fmt.Errorf("第 %d 条: %w", len(msgs), err)
		}
		msgs = append(msgs, m)
	}
	return msgs, nil
}
