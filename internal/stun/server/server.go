package server

import (
	"log"
	"net"
	"time"

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
			if nerr, ok := err.(net.Error); ok && nerr.Timeout() {
				continue
			}
			continue
		}
		s.handle(buf[:n], peer)
	}
}

func (s *Server) handle(data []byte, peer *net.UDPAddr) {
	msg, err := protocol.Parse(data)
	if err != nil {
		s.logger.Printf("parse error: %v", err)
		return
	}
	s.logger.Printf("STUN %s from %s", protocol.TypeName(msg.Type), peer.String())
	if msg.Type == protocol.TypeBindingRequest {
		// simple response
		attrs := map[uint16][]byte{
			protocol.AttrXORMappedAddress: encodeXOR(peer.IP, peer.Port),
		}
		resp, _ := buildResponse(msg, attrs)
		s.udpConn.WriteToUDP(resp, peer)
	}
}

func encodeXOR(ip net.IP, port int) []byte {
	ip4 := ip.To4()
	if ip4 == nil {
		return nil
	}
	addr := make([]byte, 8)
	addr[0] = 1 // family IPv4
	port2 := uint16(port)
	binary.BigEndian.PutUint16(addr[1:], port2)
	addr[4] = ip4[0]
	addr[5] = ip4[1]
	addr[6] = ip4[2]
	addr[7] = ip4[3]
	// simple XOR
	key := [4]byte{0x21, 0x12, 0xA4, 0x42}
	for i := 0; i < len(addr); i++ {
		addr[i] ^= key[i%4]
	}
	return addr
}

func buildResponse(req *protocol.Message, attrs map[uint16][]byte) ([]byte, error) {
	// very simple response: just type+length+cookie+txn+attrs
	// length is computed dynamically later
	msgType := protocol.TypeBindingResponse
	data := make([]byte, 0, 20+32)
	data = append(data, []byte{
		byte(msgType >> 8),
		byte(msgType),
		0, 0, // length placeholder
		byte(protocol.MagicCookie[0]),
		byte(protocol.MagicCookie[1]),
		byte(protocol.MagicCookie[2]),
		byte(protocol.MagicCookie[3]),
	}...)
	for i := 0; i < 12; i++ {
		data = append(data, 0)
	}
	msgLength := 0
	for attrType, attrVal := range attrs {
		data = append(data, []byte{
			byte(attrType >> 8),
			byte(attrType),
			byte(len(attrVal) >> 8),
			byte(len(attrVal)),
		}...)
		data = append(data, attrVal...)
		if l := len(attrVal); l%4 != 0 {
			data = append(data, make([]byte, 4-(l%4))...)
		}
		msgLength += 4 + l
		if l%4 != 0 {
			msgLength += 4 - (l % 4)
		}
	}
	data[2] = byte(msgLength >> 8)
	data[3] = byte(msgLength)
	return data, nil
}

func (s *Server) Stop() {
	if s.udpConn != nil {
		s.udpConn.Close()
	}
}
