// Package server 实现 STUN 服务器
package server

import (
	"log"
	"net"
	"time"

	"vrndis/internal/stun/protocol"
)

// Config STUN 服务器配置
type Config struct {
	ListenAddr   string // e.g. ":3478"
	RealmName    string
	OverwriteNAT bool
}

// Server STUN 服务器
type Server struct {
	cfg    Config
	udpConn *net.UDPConn
	logger *log.Logger
}

// New 创建 STUN 服务器
func New(cfg Config) *Server {
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":3478"
	}
	return &Server{
		cfg:    cfg,
		logger: log.Default(),
	}
}

// Start 启动服务器
func (s *Server) Start() error {
	udpAddr, err := net.ResolveUDPAddr("udp", s.cfg.ListenAddr)
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return err
	}
	s.udpConn = conn
	s.logger.Printf("STUN server listening on %s", s.cfg.ListenAddr)

	buffer := make([]byte, 65535)
	for {
		n, addr, err := conn.ReadFromUDP(buffer)
		if err != nil {
			if nerr, ok := err.(net.Error); ok && nerr.Timeout() {
				continue
			}
			if s.cfg.OverwriteNAT { // 实际退出的条件
				break
			}
			continue
		}
		s.handleRequest(buffer[:n], addr)
	}
	return nil
}

// handleRequest 处理 STUN 请求
func (s *Server) handleRequest(data []byte, addr *net.UDPAddr) {
	msg, err := protocol.ParseMessage(data)
	if err != nil {
		s.logger.Printf("parse stun message error: %v", err)
		return
	}

	s.logger.Printf("received STUN %s from %s", protocol.TypeName(msg.Type), addr.String())

	// 处理 Binding Request
	if msg.Type == protocol.TypeBindingRequest {
		s.handleBindingRequest(msg, addr)
	}
}

// handleBindingRequest 处理 Binding Request
func (s *Server) handleBindingRequest(req *protocol.Message, addr *net.UDPAddr) {
	// 构建 Binding Response
	mappedAddr, err := s.getMappedAddress(addr)
	if err != nil {
		s.logger.Printf("get mapped address error: %v", err)
		return
	}

	attrs := map[uint16][]byte{
		protocol.AttrXORMappedAddress: encodedXORMappedAddress(mappedAddr),
	}

	msg, err := protocol.BuildMessage(protocol.TypeBindingResponse, attrs)
	if err != nil {
		s.logger.Printf("build response error: %v", err)
		return
	}

	_, err = s.udpConn.WriteToUDP(msg, addr)
	if err != nil {
		s.logger.Printf("send response error: %v", err)
	}
}

// getMappedAddress 获取客户端的映射地址（公开 IP:Port）
func (s *Server) getMappedAddress(addr *net.UDPAddr) ([]byte, error) {
	// 在真实环境中，需要连接器 IP 和端口
	// 对于简单实现，使用客户端的公网地址信息
	ip := addr.IP.To4()
	if ip == nil {
		return nil, fmt.Errorf("invalid IP address")
	}
	port := uint16(addr.Port)
	// 构建 XOR_MAPPED_ADDRESS: Family(1) Port(2) IP(4)
	result := make([]byte, 8)
	result[0] = 1 // IPv4 family
	binary.BigEndian.PutUint16(result[1:], port)
	result[4] = ip[0]
	result[5] = ip[1]
	result[6] = ip[2]
	result[7] = ip[3]
	// XOR 密钥
	for i := 0; i < 4; i++ {
		result[i] ^= 0 // XOR with 0 for simplicity
	}
	return result, nil
}

// Stop 停止服务器
func (s *Server) Stop() {
	if s.udpConn != nil {
		s.udpConn.Close()
	}
}

// 辅助函数
func encodedXORMappedAddress(addr []byte) []byte {
	// 添加基础 XOR 密钥 0x2112A442
	key := [4]byte{0x21, 0x12, 0xA4, 0x42}
	for i := 0; i < len(addr); i++ {
		addr[i] ^= key[i%4]
	}
	return addr
}

func typeName(t uint16) string {
	switch t {
	case protocol.TypeBindingRequest:
		return "BIND_REQUEST"
	case protocol.TypeBindingResponse:
		return "BIND_RESPONSE"
	case protocol.TypeBindingError:
		return "BIND_ERROR"
	default:
		return fmt.Sprintf("TYPE_%x", t)
	}
}
