package crypto

import (
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/Re-s/vrndisd/internal/proto"
)

type pairConn struct {
	mu     sync.Mutex
	cond   *sync.Cond
	peer   *pairConn
	remain []byte
	closed bool
}

func newPair() (*pairConn, *pairConn) {
	a := &pairConn{}
	a.cond = sync.NewCond(&a.mu)
	b := &pairConn{}
	b.cond = sync.NewCond(&b.mu)
	a.peer, b.peer = b, a
	return a, b
}

var _ net.Conn = (*pairConn)(nil)

func (c *pairConn) Read(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.remain) == 0 && !c.closed {
		c.cond.Wait()
	}
	if len(c.remain) == 0 && c.closed {
		return 0, nil
	}
	n := copy(b, c.remain)
	c.remain = c.remain[n:]
	return n, nil
}

func (c *pairConn) Write(b []byte) (int, error) {
	// remain 属于 peer，必须在 peer.mu 下修改，否则与 peer 的 Read 数据竞争。
	peer := c.peer
	peer.mu.Lock()
	peer.remain = append(peer.remain, append([]byte(nil), b...)...)
	peer.mu.Unlock()
	peer.cond.Signal()
	return len(b), nil
}

func (c *pairConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.cond.Broadcast()
	return nil
}

func (c *pairConn) LocalAddr() net.Addr                { return nil }
func (c *pairConn) RemoteAddr() net.Addr               { return nil }
func (c *pairConn) SetDeadline(t time.Time) error      { return nil }
func (c *pairConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *pairConn) SetWriteDeadline(t time.Time) error { return nil }

// captureConn 记录写入的帧（用于重放/篡改测试）。
type captureConn struct {
	net.Conn
	frames [][]byte
}

func (c *captureConn) Write(b []byte) (int, error) {
	c.frames = append(c.frames, append([]byte(nil), b...))
	return c.Conn.Write(b)
}

func securePair(t *testing.T) (*SecureConn, *SecureConn) {
	t.Helper()
	a, b := newPair()
	key := [32]byte{1}
	ca, err := NewSecureConn(a, key, key)
	if err != nil {
		t.Fatal(err)
	}
	cb, err := NewSecureConn(b, key, key)
	if err != nil {
		t.Fatal(err)
	}
	return ca, cb
}

func secureWithCapture(t *testing.T) (*SecureConn, *SecureConn, *captureConn) {
	t.Helper()
	a, b := newPair()
	key := [32]byte{1}
	cc := &captureConn{Conn: a}
	ca, err := NewSecureConn(cc, key, key)
	if err != nil {
		t.Fatal(err)
	}
	cb, err := NewSecureConn(b, key, key)
	if err != nil {
		t.Fatal(err)
	}
	return ca, cb, cc
}

// writeFrame 加密写入并等待对端解码。
func writeFrame(t *testing.T, ca, cb *SecureConn, want string) {
	t.Helper()
	frame, err := ca.Encrypt([]byte(want))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		plaintext, err := cb.Decrypt(frame)
		if err != nil {
			t.Errorf("decrypt error: %v", err)
			return
		}
		if string(plaintext) != want {
			t.Errorf("mismatch: got %q want %q", plaintext, want)
		}
	}()
	if _, err := ca.Underlying().Write(frame); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}
}

func readFrame(c net.Conn) ([]byte, error) {
	buf := make([]byte, 12)
	if _, err := c.Read(buf); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint32(buf[:4]))
	ct := make([]byte, n)
	if _, err := c.Read(ct); err != nil {
		return nil, err
	}
	return append(buf, ct...), nil
}

func TestRoundTrip(t *testing.T) {
	ca, cb := securePair(t)
	defer ca.Close()
	defer cb.Close()
	writeFrame(t, ca, cb, "hello vrndis world")
}

func TestWrongKey(t *testing.T) {
	a, b := newPair()
	defer a.Close()
	defer b.Close()
	ca, err := NewSecureConn(a, [32]byte{1}, [32]byte{2})
	if err != nil {
		t.Fatal(err)
	}
	cb, err := NewSecureConn(b, [32]byte{3}, [32]byte{2})
	if err != nil {
		t.Fatal(err)
	}
	frame, err := ca.Encrypt([]byte("test"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Write(frame); err != nil {
		t.Fatal(err)
	}
	if _, err := cb.Decrypt(frame); err == nil {
		t.Error("expected decryption failure with wrong key")
	}
}

func TestReplayRejected(t *testing.T) {
	ca, cb, cc := secureWithCapture(t)
	defer ca.Close()
	defer cb.Close()
	writeFrame(t, ca, cb, "first")
	for i := 0; i < 50 && len(cc.frames) == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if len(cc.frames) == 0 {
		t.Fatal("no frame captured")
	}
	frame := cc.frames[0]
	if _, err := cc.Write(frame); err != nil {
		t.Fatal(err)
	}
	if _, err := cb.Decrypt(frame); err == nil {
		t.Fatal("expected replay rejection")
	}
}

func TestTamperDetected(t *testing.T) {
	ca, cb, cc := secureWithCapture(t)
	defer ca.Close()
	defer cb.Close()
	frame, err := ca.Encrypt([]byte("test"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cc.Write(frame); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50 && len(cc.frames) == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if len(cc.frames) == 0 {
		t.Fatal("no frame captured")
	}
	tampered := append([]byte(nil), cc.frames[0]...)
	tampered[len(tampered)-1] ^= 0xFF
	if _, err := cc.Write(tampered); err != nil {
		t.Fatal(err)
	}
	if _, err := cb.Decrypt(tampered); err == nil {
		t.Fatal("expected tamper detection")
	}
}

func TestHandshake(t *testing.T) {
	a, b := newPair()
	defer a.Close()
	defer b.Close()

	psk := []byte("my-secret-psk-1234567890123456")

	done := make(chan error)
	go func() {
		defer close(done)
		sc, err := ServerHandshake(b, psk, proto.ProtocolVersion)
		if err != nil {
			done <- err
			return
		}
		frame, err := readFrame(sc.Underlying())
		if err != nil {
			done <- err
			return
		}
		plaintext, err := sc.Decrypt(frame)
		if err != nil {
			done <- err
			return
		}
		if string(plaintext) != "hello" {
			done <- fmt.Errorf("mismatch: %s", plaintext)
		}
	}()

	sc, err := ClientHandshake(a, psk, proto.ProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := sc.Encrypt([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Write(frame); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
