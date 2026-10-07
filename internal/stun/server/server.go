package server

import (
	"encoding/binary"
	"log"
	"net"

	"vrndis/internal/stun/protocol"
)

type Config struct {
	ListenAddr string
	RealmName  string
}

type Server struct {
	cfg    Config
	udpConn *net.UDPConn
	logger *log.Logger
}

func New(cfg Config) *Server {
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":3478"
	}
	return &Server{cfg: cfg, logger: log.Default()}
}

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
			continue
		}
		msg, _ := protocol.Parse(buf[:n])
		s.logger.Printf("STUN %s from %s", protocol.TypeName(msg.Type), peer.String())
		if msg.Type == protocol.BindingRequest {
			peerBytes := encodeXOR(peer.IP, peer.Port)
			msgType := uint16(protocol.BindingResponse)
			resp := make([]byte, 20+len(peerBytes)+4)
			binary.BigEndian.PutUint16(resp[0:], msgType)
			binary.BigEndian.PutUint16(resp[2:], uint16(4+len(peerBytes)+20))
			binary.BigEndian.PutUint32(resp[4:], protocol.MagicCookie)
			copy(resp[8:20], msg.TransactionID[:])
			binary.BigEndian.PutUint16(resp[20:22], uint16(protocol.AttrXORMappedAddr))
			binary.BigEndian.PutUint16(resp[22:24], uint16(len(peerBytes)))
			copy(resp[24:], peerBytes)
			s.udpConn.WriteToUDP(resp, peer)
		}
	}
}

func encodeXOR(ip net.IP, port int) []byte {
	ip4 := ip.To4()
	if ip4 == nil {
		return nil
	}
	addr := make([]byte, 8)
	addr[0] = 1
	port2 := uint16(port)
	binary.BigEndian.PutUint16(addr[1:], port2)
	addr[4] = ip4[0]
	addr[5] = ip4[1]
	addr[6] = ip4[2]
	addr[7] = ip4[3]
	key := []byte{0x21, 0x12, 0xA4, 0x42}
	for i := 0; i < 8; i++ {
		addr[i] ^= key[i%4]
	}
	return addr
}

func (s *Server) Stop() {
	if s.udpConn != nil {
		s.udpConn.Close()
	}
}
