package server

import (
	"encoding/binary"
	"log"
	"net"
	"sync"

	"github.com/Re-s/vrndisd/internal/stun/protocol"
)

type Config struct {
	// ListenAddr 主监听地址（默认 :5478）。
	ListenAddr string
	// ExtraAddr 第二监听地址（可空）：供客户端 netcheck 做跨目的地映射
	// 比对（MappingVariesByDestPort，检测 symmetric NAT）。两个同服务器
	// 不同端口仅能识别"端口随目的地变化"的对称 NAT；跨 IP 差异需
	// 客户端额外指定第三方 STUN（-extra-stun）。
	ExtraAddr string
	RealmName string
}

type Server struct {
	cfg    Config
	conns  []*net.UDPConn
	logger *log.Logger

	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	mu     sync.Mutex
	closed bool
}

func New(cfg Config) *Server {
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":5478"
	}
	return &Server{cfg: cfg, logger: log.Default(), stopCh: make(chan struct{})}
}

// Start 启动所有已配置端口的 UDP 监听并处理 Binding Request。
// 阻塞运行直到 Stop 被调用；Stop 之后返回 nil。
func (s *Server) Start() error {
	addrs := []string{s.cfg.ListenAddr}
	if s.cfg.ExtraAddr != "" {
		addrs = append(addrs, s.cfg.ExtraAddr)
	}
	for _, a := range addrs {
		addr, err := net.ResolveUDPAddr("udp", a)
		if err != nil {
			s.closeAll()
			return err
		}
		conn, err := net.ListenUDP("udp", addr)
		if err != nil {
			s.closeAll()
			return err
		}
		s.mu.Lock()
		s.conns = append(s.conns, conn)
		s.mu.Unlock()
		s.logger.Printf("STUN server listening on %s", a)
	}
	for _, conn := range s.conns {
		s.wg.Add(1)
		go s.serve(conn)
	}
	<-s.stopCh
	return nil
}

func (s *Server) serve(conn *net.UDPConn) {
	defer s.wg.Done()
	buf := make([]byte, 65535)
	for {
		n, peer, err := conn.ReadFromUDP(buf)
		if err != nil {
			if s.isClosed() {
				return
			}
			continue
		}
		msg, _ := protocol.Parse(buf[:n])
		if msg.Type == protocol.BindingRequest {
			peerBytes := encodeXOR(peer.IP, peer.Port)
			if peerBytes == nil {
				continue
			}
			resp := make([]byte, 20+len(peerBytes)+4)
			binary.BigEndian.PutUint16(resp[0:], uint16(protocol.BindingResponse))
			binary.BigEndian.PutUint16(resp[2:], uint16(4+len(peerBytes)))
			binary.BigEndian.PutUint32(resp[4:], protocol.MagicCookie)
			copy(resp[8:20], msg.TransactionID[:])
			binary.BigEndian.PutUint16(resp[20:22], uint16(protocol.AttrXORMappedAddr))
			binary.BigEndian.PutUint16(resp[22:24], uint16(len(peerBytes)))
			copy(resp[24:], peerBytes)
			_, _ = conn.WriteToUDP(resp, peer)
		}
	}
}

// Addr 返回主端口已绑定的本地地址；未绑定时返回 nil。
func (s *Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.conns) == 0 {
		return nil
	}
	return s.conns[0].LocalAddr()
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

// Stop 优雅停止：关闭全部 UDP 监听，使 Start 返回。
// 幂等，可多次调用。
func (s *Server) Stop() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	s.stopOnce.Do(func() { close(s.stopCh) })
	s.closeAll()
	s.wg.Wait()
}

func (s *Server) closeAll() {
	for _, c := range s.conns {
		c.Close()
	}
	s.conns = nil
}

func (s *Server) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}
