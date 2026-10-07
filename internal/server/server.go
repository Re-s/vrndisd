// Package server 实现虚拟 RNDIS 服务器。
//
// 职责：监听 TCP、为客户端分配虚拟 IP/MAC、在内核侧挂 TAP 网关、
// 用学习型交换机在隧道与内核之间转发以太网帧。
package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"vrndis/internal/addr"
	"vrndis/internal/ethernet"
	"vrndis/internal/ethswitch"
	"vrndis/internal/proto"
	"vrndis/internal/tap"
)

const tapPort = "tap"

// Config 服务器配置。
type Config struct {
	Version     string   // 版本号
	Listen  string   // 监听地址，如 "0.0.0.0:7777"
	Network string   // 虚拟地址池，如 "10.77.0.0/24"
	Gateway string   // 网关（服务器自身地址），如 "10.77.0.1"
	DNS     []string // 下发给客户端的 DNS
	TapName string   // 服务器侧网卡名
	TapMAC  string   // 服务器网卡 MAC（02:00:00:00:00:00，客户端用 01-FF）
	MTU     int
	Device  tap.Device // 可选：注入设备（测试用），为 nil 时创建真实 TAP
}

// DefaultConfig 默认配置。
func DefaultConfig() Config {
	return Config{
		Listen:  "0.0.0.0:7777",
		Network: "10.77.0.0/24",
		Gateway: "10.77.0.1",
		DNS:     []string{"8.8.8.8", "1.1.1.1"},
		TapName: "vrndis0",
		TapMAC:  "02:00:00:00:00:00",
		MTU:     1500,
	}
}

// ClientInfo 客户端详细信息
type ClientInfo struct {
	IP           net.IP
	MAC          net.HardwareAddr
	Peer         string
	Name         string
	FirstConnect time.Time
	LastActive   time.Time
	LeaseID      uint64
	Prefix       int
	Gateway      net.IP
	DNS          []string
	MTU          int
	ServerMac    net.HardwareAddr
}

// Server 虚拟 RNDIS 服务器。
type Server struct {
	cfg    Config
	dev    tap.Device
	pool   *addr.Pool
	sw     *ethswitch.LearningSwitch
	tapMAC net.HardwareAddr
	ln     net.Listener
	logger *log.Logger

	mu       sync.RWMutex
	sessions map[string]*session

	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	tapQ       chan []byte // TAP 写队列：内核/网卡慢时丢帧而非停摆全网
	tapDropped atomic.Uint64

	framesIn  atomic.Uint64 // 从客户端收到的帧
	framesOut atomic.Uint64 // 发往客户端的帧

	// 客户端管理器 (仅用于管理功能)
	clientManager *ClientManager
}

type session struct {
	id      uint64
	conn    net.Conn
	peer    string
	lease   *addr.Lease
	macKey  string
	name    string
	writeMu sync.Mutex
	outQ    chan []byte // 有界发送队列：慢消费者只丢自己的帧，不阻塞交换机
	done    chan struct{}
	dropped atomic.Uint64
}

func (s *session) send(typ byte, body []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return proto.WriteMessage(s.conn, typ, body)
}

func (s *session) enqueueFrame(frame []byte) {
	buf := append([]byte(nil), frame...)
	select {
	case s.outQ <- buf:
	default:
		s.dropped.Add(1)
	}
}

// writer 把队列中的帧写回连接。
func (s *session) writer() {
	for {
		select {
		case frame := <-s.outQ:
			s.writeMu.Lock()
			err := proto.WriteMessage(s.conn, proto.TypeData, frame)
			s.writeMu.Unlock()
			if err != nil {
				return
			}
		case <-s.done:
			return
		}
	}
}

// New 创建服务器并绑定监听端口。
func New(cfg Config) (*Server, error) {
	if cfg.Listen == "" {
		cfg.Listen = "0.0.0.0:7777"
	}
	if cfg.MTU == 0 {
		cfg.MTU = 1500
	}
	if len(cfg.DNS) == 0 {
		cfg.DNS = []string{"8.8.8.8", "1.1.1.1"}
	}
	tapMAC, err := ethernet.MACFromString(cfg.TapMAC)
	if err != nil {
		return nil, err
	}
	pool, err := addr.New(cfg.Network, cfg.Gateway)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", cfg.Listen, err)
	}
	srv := &Server{
		cfg:      cfg,
		pool:     pool,
		sw:       ethswitch.New(),
		tapMAC:   tapMAC,
		ln:       ln,
		logger:   log.Default(),
		sessions: make(map[string]*session),
		stopCh:   make(chan struct{}),
	}
	srv.clientManager = NewClientManager(srv)
	return srv, nil
}

// Addr 实际监听地址。
func (s *Server) Addr() net.Addr { return s.ln.Addr() }

// Port 实际监听端口（便于测试用端口 0）。
func (s *Server) Port() int {
	if ta, ok := s.ln.Addr().(*net.TCPAddr); ok {
		return ta.Port
	}
	return 0
}

// Start 配置网卡并启动后台协程（非阻塞）。
func (s *Server) Start() error {
	dev := s.cfg.Device
	if dev == nil {
		opened, err := tap.Open(s.cfg.TapName)
		if err != nil {
			return err
		}
		dev = opened
	}
	s.dev = dev
	s.tapQ = make(chan []byte, 512)
	if err := dev.SetMAC(s.tapMAC); err != nil {
		return fmt.Errorf("set tap mac: %w", err)
	}
	if err := dev.SetIPv4(s.pool.Gateway(), s.pool.Prefix()); err != nil {
		return fmt.Errorf("set tap ipv4: %w", err)
	}
	if err := dev.Up(); err != nil {
		return fmt.Errorf("tap up: %w", err)
	}
	s.logger.Printf("vrndis server: TAP %s 就绪（网关 %s/%d），监听 %s",
		dev.Name(), s.cfg.Gateway, s.pool.Prefix(), s.ln.Addr())

	s.wg.Add(4) // 新增 localhost 心跳协程
	go s.acceptLoop()
	go s.tapLoop()
	go s.tapWriter()
	go s.clientManager.startHeartbeat()
	return nil
}

// Serve 启动并阻塞直到 Stop。
func (s *Server) Serve() error {
	if err := s.Start(); err != nil {
		return err
	}
	<-s.stopCh
	s.wg.Wait()
	return nil
}

// Stop 优雅停止。
func (s *Server) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
		s.ln.Close()
		if s.dev != nil {
			s.dev.Close()
		}
		s.mu.Lock()
		sessions := make([]*session, 0, len(s.sessions))
		for _, sess := range s.sessions {
			sessions = append(sessions, sess)
		}
		s.mu.Unlock()
		for _, sess := range sessions {
			sess.conn.Close()
		}
	})
	s.wg.Wait()
}

// SessionCount 当前在线客户端数。
func (s *Server) SessionCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.sessions)
}

// FreeAddresses 地址池剩余可用地址数（运维观测 / 泄漏检测用）。
func (s *Server) FreeAddresses() int { return s.pool.Free() }

// Stats 帧统计。
func (s *Server) Stats() (in, out uint64) {
	return s.framesIn.Load(), s.framesOut.Load()
}

// GetClients 获取所有在线客户端信息
func (s *Server) GetClients() []*ClientInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	clients := make([]*ClientInfo, 0, len(s.sessions))
	for _, sess := range s.sessions {
		lease := sess.lease
		clients = append(clients, &ClientInfo{
			IP:           lease.IP,
			MAC:          lease.MAC,
			Peer:         sess.peer,
			Name:         sess.name,
			LeaseID:      lease.ClientID,
			Prefix:       lease.Prefix,
			Gateway:      lease.Gateway,
			DNS:          s.cfg.DNS,
			MTU:          s.cfg.MTU,
			ServerMac:    s.tapMAC,
			FirstConnect: time.Now(), // 实际应该记录 connect time
			LastActive:   time.Now(),
		})
	}
	return clients
}

// SendCommandToClient 向指定客户端发送管理命令
func (s *Server) SendCommandToClient(mac string, cmd []byte) error {
	s.mu.RLock()
	sess, ok := s.sessions[mac]
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("客户端未找到 (mac=%s)", mac)
	}
	return sess.send(proto.TypeManagement, cmd)
}

// KickClient 踢出客户端
func (s *Server) KickClient(mac string) error {
	s.mu.RLock()
	sess, ok := s.sessions[mac]
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("客户端未找到 (mac=%s)", mac)
	}
	sess.conn.Close()
	return nil
}

// ---- 内部 ----

func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.stopCh:
				return
			default:
				s.logger.Printf("vrndis server: accept: %v", err)
				return
			}
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConn(conn)
		}()
	}
}

func (s *Server) handleConn(conn net.Conn) {
	peer := conn.RemoteAddr().String()
	lease, err := s.pool.Allocate()
	if err != nil {
		s.logger.Printf("vrndis server: 无法为 %s 分配地址: %v", peer, err)
		conn.Close()
		return
	}
	macKey := lease.MAC.String()
	sess := &session{
		id: lease.ClientID, conn: conn, peer: peer,
		lease: lease, macKey: macKey,
		outQ: make(chan []byte, 512),
		done: make(chan struct{}),
	}
	var writerWG sync.WaitGroup
	writerWG.Add(1)
	go func() {
		defer writerWG.Done()
		sess.writer()
	}()
	s.mu.Lock()
	s.sessions[macKey] = sess
	s.mu.Unlock()
	defer func() {
		close(sess.done)
		writerWG.Wait()
		s.mu.Lock()
		delete(s.sessions, macKey)
		s.mu.Unlock()
		s.sw.Forget(macKey)
		s.pool.Release(lease.IP)
		conn.Close()
		s.logger.Printf("vrndis server: 客户端离开 %s，回收 %s（丢帧 %d）",
			peer, lease.IP, sess.dropped.Load())
	}()

	if err := sess.send(proto.TypeAssign, mustJSON(proto.Assignment{
		Version:   proto.ProtocolVersion,
		LeaseID:   lease.ClientID,
		IP:        lease.IP.String(),
		Prefix:    lease.Prefix,
		Gateway:   lease.Gateway.String(),
		DNS:       s.cfg.DNS,
		MTU:       s.cfg.MTU,
		MAC:       lease.MAC.String(),
		ServerMAC: s.tapMAC.String(),
		ServerIP:  s.cfg.Gateway,
	})); err != nil {
		s.logger.Printf("vrndis server: 下发地址失败 %s: %v", peer, err)
		return
	}
	s.logger.Printf("vrndis server: 客户端接入 %s -> 分配 %s (%s)", peer, lease.IP, lease.MAC)

	for {
		msg, err := proto.ReadMessage(conn)
		if err != nil {
			s.logger.Printf("vrndis server: 客户端 %s 断开: %v", peer, err)
			return
		}
		switch msg.Type {
		case proto.TypeData:
			s.handleClientFrame(sess, msg.Body)
			s.framesIn.Add(1)
		case proto.TypeHello:
			var hello proto.Hello
			if json.Unmarshal(msg.Body, &hello) == nil {
				sess.name = hello.Name
			}
		case proto.TypeKeepalive:
			// 心跳，忽略
		case proto.TypePing:
			// 响应 PING
			_ = sess.send(proto.TypePong, nil)
		case proto.TypeManagement:
			// 管理消息，忽略（后续实现具体命令）
		case proto.TypeBye:
			return
		default:
			// 宽容未知类型，向前兼容
		}
	}
}

func (s *Server) handleClientFrame(sess *session, frame []byte) {
	f, err := ethernet.ParseFrame(frame)
	if err != nil {
		return // 畸形帧直接丢弃，绝不影响连接
	}
	s.sw.Learn(f.Src, sess.macKey)
	targets := s.sw.Forward(f.Dst, sess.macKey, s.allPorts())
	s.deliver(frame, targets)
}

func (s *Server) tapLoop() {
	defer s.wg.Done()
	for {
		frame, err := s.dev.Read()
		if err != nil {
			select {
			case <-s.stopCh:
			default:
				s.logger.Printf("vrndis server: TAP 读取失败: %v", err)
			}
			return
		}
		f, err := ethernet.ParseFrame(frame)
		if err != nil {
			continue
		}
		s.sw.Learn(f.Src, tapPort)
		targets := s.sw.Forward(f.Dst, tapPort, s.allPorts())
		s.deliver(frame, targets)
	}
}

func (s *Server) allPorts() map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ports := make(map[string]bool, len(s.sessions)+1)
	ports[tapPort] = true
	for key := range s.sessions {
		ports[key] = true
	}
	return ports
}

func (s *Server) deliver(frame []byte, targets map[string]bool) {
	for port := range targets {
		if port == tapPort {
			buf := append([]byte(nil), frame...)
			select {
			case s.tapQ <- buf:
			default:
				s.tapDropped.Add(1) // TAP 侧拥塞：丢帧不阻塞交换机
			}
			continue
		}
		s.mu.RLock()
		sess := s.sessions[port]
		s.mu.RUnlock()
		if sess == nil {
			continue
		}
		sess.enqueueFrame(frame)
		s.framesOut.Add(1)
	}
}

// tapWriter 独立协程把帧写入 TAP，避免内核队列满时阻塞整个交换机。
func (s *Server) tapWriter() {
	defer s.wg.Done()
	for {
		select {
		case frame := <-s.tapQ:
			if err := s.dev.Write(frame); err != nil {
				select {
				case <-s.stopCh:
					return
				default:
					s.logger.Printf("vrndis server: 写入 TAP 失败: %v", err)
				}
			}
		case <-s.stopCh:
			return
		}
	}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// ClientManager 实现客户端管理逻辑
type ClientManager struct {
	server *Server
}

func NewClientManager(server *Server) *ClientManager {
	return &ClientManager{server: server}
}

func (cm *ClientManager) startHeartbeat() {
	defer cm.server.wg.Done()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-cm.server.stopCh:
			return
		case <-ticker.C:
			cm.checkClients()
		}
	}
}

func (cm *ClientManager) checkClients() {
	cm.server.mu.RLock()
	defer cm.server.mu.RUnlock()

	for mac, sess := range cm.server.sessions {
		// 发送 PING 请求，期望 PONG 响应
		_ = sess.send(proto.TypePing, nil)
		_ = sess
		_ = mac
	}
}

// Version 服务端版本号
var Version = "1.0.0+unknown"
