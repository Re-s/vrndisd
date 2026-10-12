package server

import (
	"encoding/binary"
	"log"
	"net"
	"sync"

	"vrndis/internal/stun/protocol"
)

type Config struct {
	ListenAddr string
	RealmName  string
}

type Server struct {
	cfg     Config
	udpConn *net.UDPConn
	logger  *log.Logger

	mu     sync.Mutex
	closed bool
}

func New(cfg Config) *Server {
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":5478"
	}
	return &Server{cfg: cfg, logger: log.Default()}
}

// Start 启动 UDP 监听并处理 Binding Request。
// 阻塞运行直到 Stop 被调用；Stop 之后返回 nil。
func (s *Server) Start() error {
	addr, _ := net.ResolveUDPAddr("udp", s.cfg.ListenAddr)
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return err
	}
	s.udpConn = conn
	s.logger.Printf("STUN server listening on %s", s.cfg.ListenAddr)
	buf := make([]byte, 65535)
	for {
		n, peer, err := conn.ReadFromUDP(buf)
		if err != nil {
			// Stop 已关闭连接：优雅退出。
			if s.isClosed() {
				return nil
			}
			continue
		}
		msg, _ := protocol.Parse(buf[:n])
		s.logger.Printf("STUN %s from %s", protocol.TypeName(msg.Type), peer.String())
		if msg.Type == protocol.BindingRequest {
			peerBytes := encodeXOR(peer.IP, peer.Port)
			msgType := uint16(protocol.BindingResponse)
			resp := make([]byte, 20+len(peerBytes)+4)
			binary.BigEndian.PutUint16(resp[0:], msgType)
			binary.BigEndian.PutUint16(resp[2:], uint16(4+len(peerBytes)))
			binary.BigEndian.PutUint32(resp[4:], protocol.MagicCookie)
			copy(resp[8:20], msg.TransactionID[:])
			binary.BigEndian.PutUint16(resp[20:22], uint16(protocol.AttrXORMappedAddr))
			binary.BigEndian.PutUint16(resp[22:24], uint16(len(peerBytes)))
			copy(resp[24:], peerBytes)
			s.udpConn.WriteToUDP(resp, peer)
		}
	}
}

// Addr 返回已绑定的本地地址；未绑定时返回 nil。
func (s *Server) Addr() net.Addr {
	if s.udpConn == nil {
		return nil
	}
	return s.udpConn.LocalAddr()
}

func encodeXOR(ip net.IP, port int) []byte {
	ip4 := ip.To4()
	if ip4 == nil {
		return nil
	}
	addr := make([]byte, 8)
	addr[0] = 1 // family，不参与 XOR（RFC 5389 §15.2）
	addr[1] = 0 // 保留字节
	port2 := uint16(port)
	binary.BigEndian.PutUint16(addr[1:], port2)
	key := []byte{0x21, 0x12, 0xA4, 0x42}
	// 端口 2 字节与 key[0:2] 异或；地址 4 字节与 key[0:4] 异或。
	addr[1] ^= key[0]
	addr[2] ^= key[1]
	addr[4] = ip4[0] ^ key[0]
	addr[5] = ip4[1] ^ key[1]
	addr[6] = ip4[2] ^ key[2]
	addr[7] = ip4[3] ^ key[3]
	return addr
}

// Stop 优雅停止：关闭 UDP 连接，使 Start 返回。
// 幂等，可多次调用。
func (s *Server) Stop() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	if s.udpConn != nil {
		s.udpConn.Close()
	}
}

func (s *Server) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}
