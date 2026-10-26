// Package crypto AEAD 加密通道（会话层 AEAD）。
//
// 用法：
//
//  1. 握手前通过 net.Conn 交换握手消息（proto.WriteMessage/ReadMessage）。
//  2. 握手成功后调用 NewSecureConn 获得 SecureConn。
//  3. 后续所有 proto.Message 经 Encrypt/Decrypt 加解密后，再以 TypeData
//     承载密文通过底层连接发送（见 session.send / client.sockToDev）。
//
// 线路格式（每条消息）：
//
//	len(u32 大端) | seq(u64 大端) | ciphertext(AES-256-GCM)
//
// seq（8 字节）同时作为 AAD 被 GCM 认证，并作为重放窗口索引。
// 明文 = type(1 byte) || body，即调用方传入的完整消息。
//
// nonce = counter(8 bytes) || 0(4 bytes)
//
//	counter：每发出一条消息自增 1，uint64 范围溢出可忽略。
//	后 4 字节填充至 GCM 要求的 12 字节。
//
// 安全性说明：GCM 要求每个 (key, nonce) 只加密一次明文。本连接中每个
// 方向各持有一个独立的 key，且每个方向的 seq 单调递增，因此
// (key, nonce) 自然唯一，无需额外 direction 前缀。
//
// 重放保护：64 包滑动窗口。seq <= lastSeq - 64 的包直接丢弃。
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// SecureConn 一条已建立密钥的加密通道。
// 它不是 net.Conn：加解密在会话层显式完成，避免与 proto 线路格式冲突。
type SecureConn struct {
	mu sync.Mutex

	conn net.Conn // 底层连接（用于 Close 等）

	writeKey, readKey [32]byte
	writeSeq, readSeq uint64

	writeCipher, readCipher cipher.AEAD

	replayWindow []bool
	lastSeq      uint64
	windowSize   int
	hasSeen      bool // 标记是否已收到过任何包（见 Decrypt 注释）
}

// NewSecureConn 由 writeKey / readKey 构造加密通道。
// conn 为底层连接，用于 Close/LocalAddr/RemoteAddr 等。
func NewSecureConn(conn net.Conn, writeKey, readKey [32]byte) (*SecureConn, error) {
	writeCipher, err := aes.NewCipher(writeKey[:])
	if err != nil {
		return nil, err
	}
	writeGCM, err := cipher.NewGCM(writeCipher)
	if err != nil {
		return nil, err
	}
	readCipher, err := aes.NewCipher(readKey[:])
	if err != nil {
		return nil, err
	}
	readGCM, err := cipher.NewGCM(readCipher)
	if err != nil {
		return nil, err
	}
	return &SecureConn{
		conn:         conn,
		writeCipher:  writeGCM,
		readCipher:   readGCM,
		writeKey:     writeKey,
		readKey:      readKey,
		windowSize:   64,
		replayWindow: make([]bool, 64),
	}, nil
}

// Underlying 返回底层连接，用于直接注入/篡改帧做测试。
func (c *SecureConn) Underlying() net.Conn { return c.conn }

// nonce 构造：seq 大端编码 + 4 字节零填充 = 12 字节。
func (c *SecureConn) nonce(seq uint64) [12]byte {
	var n [12]byte
	for i := 0; i < 8; i++ {
		n[i] = byte(seq >> (56 - 8*i))
	}
	return n
}

// Encrypt 将明文（首字节为消息 type）加密为线路帧。
// 返回的帧可以直接通过底层 net.Conn 写入。
func (c *SecureConn) Encrypt(plaintext []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(plaintext) == 0 {
		return nil, errors.New("github.com/Re-s/vrndisd/crypto: empty plaintext")
	}
	seq := c.writeSeq
	c.writeSeq++

	aad := make([]byte, 8)
	binary.BigEndian.PutUint64(aad, seq)
	nv := c.nonce(seq)
	ciphertext := c.writeCipher.Seal(nil, nv[:], plaintext, aad)

	hdr := make([]byte, 12)
	binary.BigEndian.PutUint32(hdr[:4], uint32(len(ciphertext)))
	binary.BigEndian.PutUint64(hdr[4:12], seq)
	return append(hdr, ciphertext...), nil
}

// Decrypt 从线路帧解密出明文。
// 帧格式：len(u32) | seq(u8 大端) | ciphertext
// 返回的明文首字节为原始消息 type。
func (c *SecureConn) Decrypt(frame []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(frame) < 12 {
		return nil, fmt.Errorf("github.com/Re-s/vrndisd/crypto: frame too short: %d", len(frame))
	}
	n := binary.BigEndian.Uint32(frame[:4])
	if n > maxCipherLen {
		return nil, errors.New("github.com/Re-s/vrndisd/crypto: ciphertext too large")
	}
	if uint32(len(frame)-12) != n {
		return nil, fmt.Errorf("github.com/Re-s/vrndisd/crypto: ciphertext length mismatch: declared %d, got %d", n, len(frame)-12)
	}
	seq := binary.BigEndian.Uint64(frame[4:12])
	ciphertext := frame[12:]

	aad := make([]byte, 8)
	binary.BigEndian.PutUint64(aad, seq)
	nv := c.nonce(seq)
	plaintext, err := c.readCipher.Open(nil, nv[:], ciphertext, aad)
	if err != nil {
		return nil, err
	}
	// 重放检查
	if !c.hasSeen {
		c.hasSeen = true
		c.lastSeq = seq
	} else if seq > c.lastSeq {
		shift := seq - c.lastSeq
		if shift > uint64(c.windowSize) {
			c.replayWindow = make([]bool, c.windowSize)
		} else {
			copy(c.replayWindow[shift:], c.replayWindow)
			for i := 0; i < int(shift); i++ {
				c.replayWindow[i] = false
			}
		}
		c.lastSeq = seq
	} else if seq == c.lastSeq {
		return nil, fmt.Errorf("%w: seq %d duplicate", ErrReplay, seq)
	} else {
		idx := c.lastSeq - seq
		if idx >= uint64(c.windowSize) {
			return nil, fmt.Errorf("%w: seq %d too old", ErrReplay, seq)
		}
		if c.replayWindow[idx] {
			return nil, fmt.Errorf("%w: seq %d duplicate", ErrReplay, seq)
		}
		c.replayWindow[idx] = true
	}

	return plaintext, nil
}

const maxCipherLen = 256 * 1024 // 密文长度上限

// Close 关闭底层连接。
func (c *SecureConn) Close() error { return c.conn.Close() }

// LocalAddr / RemoteAddr / SetDeadline 等透传。
func (c *SecureConn) LocalAddr() net.Addr                { return c.conn.LocalAddr() }
func (c *SecureConn) RemoteAddr() net.Addr               { return c.conn.RemoteAddr() }
func (c *SecureConn) SetDeadline(t time.Time) error      { return c.conn.SetDeadline(t) }
func (c *SecureConn) SetReadDeadline(t time.Time) error  { return c.conn.SetReadDeadline(t) }
func (c *SecureConn) SetWriteDeadline(t time.Time) error { return c.conn.SetWriteDeadline(t) }

var (
	ErrReplay = errors.New("github.com/Re-s/vrndisd/crypto: replayed message")
)
