// Package ctrl 到服务器的控制信道会话（TCP + 可选 PSK 加密），
// 供 ping / status 等诊断命令复用：完成拨号、加密握手与 Hello/Assign，
// 但不建立虚拟网卡、不进入数据面。
package ctrl

import (
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"vrndis/internal/crypto"
	"vrndis/internal/proto"
)

// Session 一条到服务器的控制信道。
type Session struct {
	conn    net.Conn
	sc      *crypto.SecureConn
	writeMu sync.Mutex
}

// Dial 建立控制信道：TCP 拨号 → 可选 PSK 加密握手 → Hello → 读 Assign。
// 返回的会话可用于 PeerQuery 查询与 Ping/Pong RTT 测量。
func Dial(server, name string, psk []byte, timeout time.Duration) (*Session, *proto.Assignment, error) {
	conn, err := net.DialTimeout("tcp", server, timeout)
	if err != nil {
		return nil, nil, fmt.Errorf("dial %s: %w", server, err)
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	s := &Session{conn: conn}
	if len(psk) > 0 {
		secure, err := crypto.ClientHandshake(conn, psk, proto.ProtocolVersion)
		if err != nil {
			conn.Close()
			return nil, nil, fmt.Errorf("加密握手失败: %w", err)
		}
		s.sc = secure
	}
	hello, _ := json.Marshal(proto.Hello{Version: proto.ProtocolVersion, Name: name})
	if err := s.Send(proto.TypeHello, hello); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("send hello: %w", err)
	}
	msg, err := s.RecvDeadline(time.Now().Add(timeout))
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("read assignment: %w", err)
	}
	if msg.Type != proto.TypeAssign {
		conn.Close()
		return nil, nil, fmt.Errorf("期望 ASSIGN，收到 %s", proto.TypeName(msg.Type))
	}
	var assign proto.Assignment
	if err := json.Unmarshal(msg.Body, &assign); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("解析 ASSIGN: %w", err)
	}
	return s, &assign, nil
}

// Send 发送一条消息（加密模式自动封进 TypeData 信封）。
func (s *Session) Send(typ byte, body []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.sc != nil {
		plaintext := append([]byte{typ}, body...)
		frame, err := s.sc.Encrypt(plaintext)
		if err != nil {
			return err
		}
		return proto.WriteMessage(s.conn, proto.TypeData, frame)
	}
	return proto.WriteMessage(s.conn, typ, body)
}

// Recv 读取一条消息（自动信封解包）。
func (s *Session) Recv() (*proto.Message, error) {
	msg, err := proto.ReadMessage(s.conn)
	if err != nil {
		return nil, err
	}
	m := s.unwrap(&msg)
	return m, nil
}

// RecvDeadline 读取一条消息，带截止时间。
func (s *Session) RecvDeadline(dl time.Time) (*proto.Message, error) {
	_ = s.conn.SetReadDeadline(dl)
	defer s.conn.SetReadDeadline(time.Time{})
	return s.Recv()
}

func (s *Session) unwrap(msg *proto.Message) *proto.Message {
	if s.sc != nil {
		pt, err := s.sc.Decrypt(msg.Body)
		if err != nil {
			return msg
		}
		msg.Type = pt[0]
		msg.Body = pt[1:]
	}
	return msg
}

// Close 关闭控制信道。
func (s *Session) Close() error {
	return s.conn.Close()
}

// PingRTT 走控制信道（Ping→Pong）测往返延迟；中继路径基准
// （数据中继与控制信道走同一服务器路径，RTT 近似）。
func (s *Session) PingRTT(timeout time.Duration) (time.Duration, error) {
	start := time.Now()
	if err := s.Send(proto.TypePing, nil); err != nil {
		return 0, err
	}
	dl := time.Now().Add(timeout)
	for {
		msg, err := s.RecvDeadline(dl)
		if err != nil {
			return 0, err
		}
		if msg.Type == proto.TypePong {
			return time.Since(start), nil
		}
		if msg.Type == proto.TypePing {
			// 服务器侧探活，忽视
			continue
		}
		// 其余消息（PeerCandidate 等夹带）忽视继续等
	}
}
