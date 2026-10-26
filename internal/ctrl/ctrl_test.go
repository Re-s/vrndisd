package ctrl

import (
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/Re-s/vrndisd/internal/crypto"
	"github.com/Re-s/vrndisd/internal/proto"
)

// runFakeServer 实现最小协议对端：Hello→Assign、Ping→Pong、
// PeerQuery→PeerCandidate，覆盖 ctrl 命令所需全部交互。
func runFakeServer(t *testing.T, psk []byte) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		conn, err := ln.Accept()
		ln.Close()
		if err != nil {
			return
		}
		defer conn.Close()
		var sc *crypto.SecureConn
		if len(psk) > 0 {
			sc, err = crypto.ServerHandshake(conn, psk, proto.ProtocolVersion)
			if err != nil {
				return
			}
		}
		recv := func() (*proto.Message, error) {
			m, err := proto.ReadMessage(conn)
			if err != nil {
				return nil, err
			}
			if sc != nil {
				pt, err := sc.Decrypt(m.Body)
				if err != nil {
					return nil, err
				}
				m.Type, m.Body = pt[0], pt[1:]
			}
			return &m, nil
		}
		send := func(typ byte, b []byte) error {
			if sc != nil {
				frame, err := sc.Encrypt(append([]byte{typ}, b...))
				if err != nil {
					return err
				}
				return proto.WriteMessage(conn, proto.TypeData, frame)
			}
			return proto.WriteMessage(conn, typ, b)
		}
		// 1) 读 Hello 回 Assign
		m, err := recv()
		if err != nil || m.Type != proto.TypeHello {
			return
		}
		body, _ := json.Marshal(proto.Assignment{
			Version: proto.ProtocolVersion,
			IP:      "10.77.0.99",
			Prefix:  24,
			Gateway: "10.77.0.1",
		})
		if err := send(proto.TypeAssign, body); err != nil {
			return
		}
		// 2) 业务循环
		for {
			m, err := recv()
			if err != nil {
				return
			}
			switch m.Type {
			case proto.TypePing:
				_ = send(proto.TypePong, nil)
			case proto.TypePeerQuery:
				body, _ := json.Marshal(proto.PeerCandidate{
					Version:   proto.ProtocolVersion,
					PeerMAC:   "aa:bb:cc:dd:ee:ff",
					Candidate: "127.0.0.1:9999",
				})
				_ = send(proto.TypePeerCandidate, body)
			case proto.TypeBye:
				return
			}
		}
	}()
	return ln.Addr().String()
}

func TestDialAssignAndPingRTT(t *testing.T) {
	var mu sync.Mutex
	srv := runFakeServer(t, nil)
	sess, assign, err := Dial(srv, "test", nil, 5*time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer sess.Close()
	if assign.IP != "10.77.0.99" {
		t.Errorf("assign IP=%q, want 10.77.0.99", assign.IP)
	}
	rtt, err := sess.PingRTT(3 * time.Second)
	if err != nil {
		t.Fatalf("PingRTT: %v", err)
	}
	if rtt <= 0 || rtt > time.Second {
		t.Errorf("rtt=%v 异常", rtt)
	}
	mu.Lock()
	defer mu.Unlock()
}

// TestDialAndQueryPlaintext 验证明文会话上 PeerQuery 全链路：
// Send(PeerQuery) → RecvDeadline(PeerCandidate)。
func TestDialAndQueryPlaintext(t *testing.T) {
	srv := runFakeServer(t, nil)
	sess, _, err := Dial(srv, "test", nil, 5*time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer sess.Close()
	body, _ := json.Marshal(proto.PeerQuery{Version: proto.ProtocolVersion, Target: "10.77.0.5"})
	if err := sess.Send(proto.TypePeerQuery, body); err != nil {
		t.Fatalf("Send: %v", err)
	}
	msg, err := sess.RecvDeadline(time.Now().Add(3 * time.Second))
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if msg.Type != proto.TypePeerCandidate {
		t.Fatalf("type=%s, want PEERCANDIDATE", proto.TypeName(msg.Type))
	}
	var c proto.PeerCandidate
	if err := json.Unmarshal(msg.Body, &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.Candidate != "127.0.0.1:9999" {
		t.Errorf("candidate=%q", c.Candidate)
	}
}

// TestDialSecure 全程 PSK 加密：握手 + Assign + Ping 均走密文。
func TestDialSecure(t *testing.T) {
	psk := []byte("test-psk")
	srv := runFakeServer(t, psk)
	sess, assign, err := Dial(srv, "test", psk, 5*time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer sess.Close()
	if assign.IP != "10.77.0.99" {
		t.Errorf("assign IP=%q", assign.IP)
	}
	if _, err := sess.PingRTT(3 * time.Second); err != nil {
		t.Fatalf("PingRTT: %v", err)
	}
}
