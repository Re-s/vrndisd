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
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"vrndis/internal/addr"
	"vrndis/internal/crypto"
	"vrndis/internal/ethernet"
	"vrndis/internal/ethswitch"
	"vrndis/internal/proto"
	"vrndis/internal/tap"
)

const tapPort = "tap"

// 服务端版本号
var Version = "1.1.0+unknown"

// Config 服务器配置。
type Config struct {
	Listen  string   // 监听地址，如 "0.0.0.0:7777"
	Network string   // 虚拟地址池，如 "10.77.0.0/24"
	Gateway string   // 网关（服务器自身地址），如 "10.77.0.1"
	DNS     []string // 下发给客户端的 DNS
	TapName string   // 服务器侧网卡名
	TapMAC  string   // 服务器网卡 MAC（02:00:00:00:00:00，客户端用 01-FF）
	MTU     int
	Device  tap.Device // 可选：注入设备（测试用），为 nil 时创建真实 TAP

	PSK []byte // 预共享密钥；非空时要求客户端先完成加密握手

	// P2P 是否开启 P2P 直连（默认关闭，纯中继，兼容旧客户端）
	P2P bool

	// ManagementSocket 本机管理套接字路径（clients/status 命令实时查询，
	// 不走客户端协议、无需 PSK）。为空时用平台默认路径；"-" 表示禁用。
	ManagementSocket string
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
	IP        net.IP
	MAC       net.HardwareAddr
	Peer      string
	Name      string
	LeaseID   uint64
	Prefix    int
	Gateway   net.IP
	DNS       []string
	MTU       int
	ServerMac net.HardwareAddr
}

// peerInfo 一个已上线的 P2P 对端（服务器侧视角）。
type peerInfo struct {
	mac       string // 虚拟 MAC（同时也是 peers 表键）
	ip        net.IP
	name      string
	candidate string   // "host:port"，该 peer 上报的 NAT 映射地址
	sess      *session // 推送候选用（复用带锁的 send）
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
	mgmtLn net.Listener // 本机管理套接字（clients/status 查询）

	mu       sync.RWMutex
	sessions map[string]*session

	// P2P peer 注册表：虚拟 MAC -> peer 状态（候选地址 + 连接）
	// 用于协调两个客户端交换候选并触发打洞。
	peers   map[string]*peerInfo
	peersMu sync.RWMutex

	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	tapQ       chan []byte // TAP 写队列：内核/网卡慢时丢帧而非停摆全网
	tapDropped atomic.Uint64

	framesIn  atomic.Uint64 // 从客户端收到的帧
	framesOut atomic.Uint64 // 发往客户端的帧
}

type session struct {
	id     uint64
	conn   net.Conn
	peer   string
	lease  *addr.Lease
	macKey string
	name   string
	// P2P 状态（管理面展示用）
	p2pCandidate string // STUN 映射候选（"" 表示未上报）
	p2pReady     bool   // 客户端通知打洞成功
	p2pPeerMAC   string // 打通的对端虚拟 MAC
	p2pMu        sync.RWMutex
	writeMu      sync.Mutex
	outQ         chan []byte // 有界发送队列：慢消费者只丢自己的帧，不阻塞交换机
	done         chan struct{}
	dropped      atomic.Uint64
	crypto       *crypto.SecureConn // 加密通道（PSK 握手后非 nil）
}

func (s *session) send(typ byte, body []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.crypto != nil {
		// 信封模式：[type]+body 整体加密，外层统一 TypeData
		plaintext := append([]byte{typ}, body...)
		encrypted, err := s.crypto.Encrypt(plaintext)
		if err != nil {
			return err
		}
		return proto.WriteMessage(s.conn, proto.TypeData, encrypted)
	}
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
			outFrame := frame
			if s.crypto != nil {
				// 信封模式：[TypeData]+frame 整体加密
				plaintext := append([]byte{proto.TypeData}, frame...)
				encrypted, encErr := s.crypto.Encrypt(plaintext)
				if encErr == nil {
					outFrame = encrypted
				}
			}
			err := proto.WriteMessage(s.conn, proto.TypeData, outFrame)
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
	return &Server{
		cfg:      cfg,
		pool:     pool,
		sw:       ethswitch.New(),
		tapMAC:   tapMAC,
		ln:       ln,
		logger:   log.Default(),
		sessions: make(map[string]*session),
		peers:    make(map[string]*peerInfo),
		stopCh:   make(chan struct{}),
	}, nil
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

// listenCandidate 服务器侧候选地址（host:port）。
// P2P 打洞采用"双方均向服务器监听端口发包"的对称打洞策略：客户端拿到该候选后
// 向服务器监听地址发 UDP 探测，NAT 建立映射；另一侧同理。NAT 通常复用同一映射，
// 从而使两侧可以直接互通。
func (s *Server) listenCandidate() string {
	return s.cfg.Listen
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

	// Windows wintun 为三层驱动：ARP 帧由 l2l3 拦截并构造 Reply，
	// 需要通过交换机把 ARP Reply 送回发起请求的客户端。
	// 若不注入，ARP Reply 被静默丢弃，客户端无法解析网关 MAC（ping 不通）。
	if arper, ok := dev.(interface{ SetARPInject(func(frame []byte)) }); ok {
		arper.SetARPInject(func(frame []byte) {
			f, err := ethernet.ParseFrame(frame)
			if err != nil {
				return
			}
			s.sw.Learn(f.Src, tapPort)
			targets := s.sw.Forward(f.Dst, tapPort, s.allPorts())
			s.deliver(frame, targets)
		})
	}
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

	s.wg.Add(4)
	go s.acceptLoop()
	go s.tapLoop()
	go s.manageLoop()
	go s.tapWriter()
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
		if s.mgmtLn != nil {
			s.mgmtLn.Close()
		}
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

// defaultManagementSocket 平台默认管理套接字路径。
func defaultManagementSocket() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(os.Getenv("ProgramData"), "vrndisd", "management.sock")
	}
	return "/run/vrndisd/management.sock"
}

// managementSocketPath 解析管理套接字路径（"-" 禁用返回空）。
func (s *Server) managementSocketPath() string {
	switch {
	case s.cfg.ManagementSocket == "-":
		return ""
	case s.cfg.ManagementSocket != "":
		return s.cfg.ManagementSocket
	default:
		return defaultManagementSocket()
	}
}

// managementState 生成当前实时管理状态快照（进程内数据，无缓存）。
func (s *Server) managementState() proto.ManagementState {
	s.mu.RLock()
	list := make([]proto.ClientInfo, 0, len(s.sessions))
	for _, sess := range s.sessions {
		lease := sess.lease
		sess.p2pMu.RLock()
		cand, ready, peerMAC := sess.p2pCandidate, sess.p2pReady, sess.p2pPeerMAC
		sess.p2pMu.RUnlock()
		var p2p string
		switch {
		case !s.cfg.P2P:
			p2p = "off"
		case ready:
			p2p = "p2p-ok -> " + peerMAC
		case cand != "":
			p2p = "stun-ok " + cand + "（打洞中）"
		default:
			p2p = "relay"
		}
		list = append(list, proto.ClientInfo{
			IP:   lease.IP.String(),
			MAC:  lease.MAC.String(),
			Peer: sess.peer,
			Name: sess.name,
			P2P:  p2p,
		})
	}
	s.mu.RUnlock()
	return proto.ManagementState{
		Version:   Version,
		UpdatedAt: time.Now(),
		Clients:   list,
	}
}

// managementWatchInterval 管理套接字自愈检测间隔。
// socket 文件被外部删除（云安全 agent / 运维脚本 / tmpfiles 清理）后，
// 旧 listener 仍 bind 但不可达，按此间隔检测并重建。包级变量便于测试调小。
var managementWatchInterval = 10 * time.Second

// manageLoop 监听本机管理套接字，响应 clients/status 查询。
// 每个连接读一行 ManagementRequest JSON，回 ManagementState JSON。
// socket 文件被外部删除时自愈重建（不退出 goroutine，保证持续可查询）。
func (s *Server) manageLoop() {
	defer s.wg.Done()
	path := s.managementSocketPath()
	if path == "" {
		return
	}
	os.MkdirAll(filepath.Dir(path), 0755)
	for {
		ln := s.listenManagement(path)
		if ln == nil {
			// 监听失败（路径占用等）：等待后重试，不退出
			select {
			case <-s.stopCh:
				return
			case <-time.After(managementWatchInterval):
			}
			continue
		}
		if !s.acceptManagement(ln, path) {
			return // 收到 stop 信号：退出（不删除 socket 文件）
		}
	}
}

// listenManagement 创建（或重建）管理套接字 listener 并记录到 s.mgmtLn。
// 每次调用先清理残留 socket 文件（systemd restart 竞态由"启动时清理 +
// 退出不删除"组合避免，见 50cacf1；外部删除后的孤儿由本函数重建）。
func (s *Server) listenManagement(path string) net.Listener {
	os.Remove(path) // 清理上次残留
	ln, err := net.Listen("unix", path)
	if err != nil {
		s.logger.Printf("vrndis server: 管理套接字监听失败 %s: %v", path, err)
		return nil
	}
	// UnixListener.Close 默认会 unlink socket 文件：restart 时序中旧进程的
	// Close 可能晚于新进程的 Listen，从而误删新进程的文件（50cacf1 移除了
	// 显式 defer Remove 却未堵住此路径，socket 仍然"消失"）。显式关闭该行为，
	// 删除完全由"启动时清理残留"承担。
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	s.mgmtLn = ln
	s.logger.Printf("vrndis server: 管理套接字 %s", path)
	return ln
}

// acceptManagement 接受管理查询连接。
// 返回 false 表示收到 stop 信号（manageLoop 应退出）；
// 返回 true 表示 listener 不可用（socket 被外部删除或意外关闭），应重建。
func (s *Server) acceptManagement(ln net.Listener, path string) bool {
	// unix listener 支持 SetDeadline：周期唤醒检测 socket 文件是否被外部删除。
	// 非 unix listener（不会发生）退化为阻塞 accept，无自愈。
	ul, hasDeadline := ln.(*net.UnixListener)
	for {
		if hasDeadline {
			_ = ul.SetDeadline(time.Now().Add(managementWatchInterval))
		}
		conn, err := ln.Accept()
		if err == nil {
			s.handleManagementConn(conn)
			continue
		}
		select {
		case <-s.stopCh:
			ln.Close()
			return false
		default:
		}
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			// 自愈检测：listener 仍 bind 但 socket 文件被外部删除
			if _, serr := os.Stat(path); serr != nil {
				s.logger.Printf("vrndis server: 管理套接字 %s 被外部删除，重建", path)
				ln.Close()
				return true
			}
			continue
		}
		// 非超时错误（listener 被关闭等）：重建
		return true
	}
}

// handleManagementConn 处理单个管理查询连接（同步短连接）。
func (s *Server) handleManagementConn(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var req proto.ManagementRequest
	dec := json.NewDecoder(conn)
	if err := dec.Decode(&req); err != nil {
		return // 非法请求直接关闭
	}
	// 所有命令统一返回完整实时状态快照（clients 命令取 .Clients）。
	state := s.managementState()
	json.NewEncoder(conn).Encode(state)
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
			IP:        lease.IP,
			MAC:       lease.MAC,
			Peer:      sess.peer,
			Name:      sess.name,
			LeaseID:   lease.ClientID,
			Prefix:    lease.Prefix,
			Gateway:   lease.Gateway,
			DNS:       s.cfg.DNS,
			MTU:       s.cfg.MTU,
			ServerMac: s.tapMAC,
		})
	}
	return clients
}

// getManagementClientInfo 获取用于管理响应的客户端信息
func (s *Server) getManagementClientInfo() []proto.ClientInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := make([]proto.ClientInfo, 0, len(s.sessions))
	for _, sess := range s.sessions {
		lease := sess.lease
		sess.p2pMu.RLock()
		cand, ready, peerMAC := sess.p2pCandidate, sess.p2pReady, sess.p2pPeerMAC
		sess.p2pMu.RUnlock()
		var p2p string
		switch {
		case !s.cfg.P2P:
			p2p = "off"
		case ready:
			p2p = "p2p-ok -> " + peerMAC
		case cand != "":
			p2p = "stun-ok " + cand + "（打洞中）"
		default:
			p2p = "relay"
		}
		list = append(list, proto.ClientInfo{
			IP:   lease.IP.String(),
			MAC:  lease.MAC.String(),
			Peer: sess.peer,
			Name: sess.name,
			P2P:  p2p,
		})
	}
	return list
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
	var sec *crypto.SecureConn
	if len(s.cfg.PSK) > 0 {
		var err error
		sec, err = crypto.ServerHandshake(conn, s.cfg.PSK, proto.ProtocolVersion)
		if err != nil {
			s.logger.Printf("vrndis server: 加密握手失败 %s: %v", peer, err)
			conn.Close()
			return
		}
		s.logger.Printf("vrndis server: 加密连接建立 %s", peer)
	}
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
		outQ:   make(chan []byte, 512),
		done:   make(chan struct{}),
		crypto: sec,
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
		s.unregisterPeer(macKey)
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
		if sess.crypto != nil && len(msg.Body) > 0 {
			plain, decErr := sess.crypto.Decrypt(msg.Body)
			if decErr != nil {
				s.logger.Printf("vrndis server: 解密失败 %s: %v", peer, decErr)
				return
			}
			if len(plain) == 0 {
				s.logger.Printf("vrndis server: 解密得到空数据 %s", peer)
				return
			}
			// 解包信封：plain[0]=真实类型, plain[1:]=真实body
			msg.Type = plain[0]
			msg.Body = plain[1:]
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
			// 管理消息
			var req proto.ManagementRequest
			if json.Unmarshal(msg.Body, &req) == nil {
				var resp proto.ManagementResponse
				switch req.Cmd {
				case "status":
					resp.Version = Version
					resp.Clients = s.SessionCount()
					respBytes, _ := json.Marshal(resp)
					sess.send(proto.TypeManagement, respBytes)
				case "clients":
					resp.List = s.getManagementClientInfo()
					resp.Version = Version
					resp.Clients = len(resp.List)
					respBytes, _ := json.Marshal(resp)
					sess.send(proto.TypeManagement, respBytes)
				default:
					// 不支持的命令
				}
			}
		case proto.TypeP2PAdvertise:
			// 客户端上报本机 NAT 候选；服务器负责协调交换：把该候选下发给对端
			if !s.cfg.P2P {
				break // P2P 未开启，忽略
			}
			var adv proto.P2PAdvertise
			if json.Unmarshal(msg.Body, &adv) != nil {
				break
			}
			if adv.MAC == "" || adv.Candidate == "" {
				break
			}
			s.registerPeer(sess, sess.lease.MAC.String(), adv.Candidate)
			// 找到对端并下发其候选
			s.distributeCandidates(sess, adv.MAC)
		case proto.TypeP2PWithdraw:
			// 客户端撤销本机候选（打洞失败/回退中继）：从 peer 表移除，
			// 避免死候选残留（后来节点会向已关闭的端点空打洞 30 秒）。
			if !s.cfg.P2P {
				break
			}
			var w proto.P2PWithdraw
			if json.Unmarshal(msg.Body, &w) != nil {
				break
			}
			mac := sess.lease.MAC.String()
			s.unregisterPeer(mac)
			sess.p2pMu.Lock()
			sess.p2pCandidate = ""
			sess.p2pReady = false
			sess.p2pPeerMAC = ""
			sess.p2pMu.Unlock()
			s.logger.Printf("vrndis server: peer %s 撤销候选（回退中继）", mac)
		case proto.TypeP2PReady:
			// 客户端通知打洞成功（管理面展示用，不影响数据面）
			if !s.cfg.P2P {
				break
			}
			var ready proto.P2PReady
			if json.Unmarshal(msg.Body, &ready) != nil {
				break
			}
			sess.p2pMu.Lock()
			sess.p2pReady = true
			sess.p2pPeerMAC = ready.PeerMAC
			sess.p2pMu.Unlock()
			s.logger.Printf("vrndis server: peer %s P2P 打洞成功 -> %s",
				sess.lease.MAC.String(), ready.PeerMAC)
		case proto.TypePeerQuery:
			// 查询 peer 候选（ping/status 命令用）；P2P 关闭时无意义
			var q proto.PeerQuery
			if json.Unmarshal(msg.Body, &q) != nil {
				break
			}
			s.answerPeerQuery(sess, q.Target)
		case proto.TypeBye:
			return
		default:
			// 宽容未知类型，向前兼容
		}
	}
}

// ---- P2P ----

// registerPeer 登记一个 peer 的候选地址。
// unregisterPeer 清除已下线的 peer 记录。
func (s *Server) unregisterPeer(mac string) {
	s.peersMu.Lock()
	defer s.peersMu.Unlock()
	delete(s.peers, mac)
	s.logger.Printf("vrndis server: 注销 peer %s", mac)
}

// candidateHost 从 "host:port" 候选串中取出 host 部分。
func candidateHost(candidate string) string {
	host, _, err := net.SplitHostPort(candidate)
	if err != nil {
		return ""
	}
	return host
}

func (s *Server) registerPeer(sess *session, mac, candidate string) {
	// 拒绝隧道网段候选：客户端 STUN 请求若被路由进隧道，服务器看到的源是
	// 隧道内网地址（如 10.77.0.6:33395）。该端口只是 STUN 反射的源端口，
	// 对端本机并未监听；向隧道地址发包只能绕中继且必然丢弃，是挤占全网
	// 打洞预算的废候选。LAN 地址（192.168.x 等）不在此列——同 NAT 设备
	// 可借 LAN 候选内网直连，绕开 hairpin 缺失。
	if host := candidateHost(candidate); host != "" {
		if ip := net.ParseIP(host); ip != nil && s.pool.Contains(ip) {
			s.logger.Printf("vrndis server: 拒绝注册隧道地址候选 %s -> %s", mac, candidate)
			return
		}
	}
	s.peersMu.Lock()
	s.peers[mac] = &peerInfo{
		mac:       mac,
		ip:        sess.lease.IP,
		name:      sess.name,
		candidate: candidate,
		sess:      sess,
	}
	sess.p2pMu.Lock()
	sess.p2pCandidate = candidate
	sess.p2pMu.Unlock()
	s.peersMu.Unlock()
	s.logger.Printf("vrndis server: 候选注册 %s -> %s", mac, candidate)
	// 向其它已上报候选的在线 peer 推送本端新候选。
	// 客户端两端上线时序错开时，只有此处推送才能让对端在打洞窗口内
	// 拿到本端候选（distributeCandidates 只在新 session 上线时下发）。
	s.pushCandidateToOthers(sess, mac, candidate)
}

// pushCandidateToOthers 把新注册的候选推送给其它已在线且已上报候选的 peer。
func (s *Server) pushCandidateToOthers(sess *session, selfMAC, candidate string) {
	s.peersMu.RLock()
	self := s.peers[selfMAC]
	others := make([]*peerInfo, 0, len(s.peers))
	for _, p := range s.peers {
		if p.mac != selfMAC && p.candidate != "" && p.sess != nil {
			others = append(others, p)
		}
	}
	s.peersMu.RUnlock()
	if self == nil {
		return
	}
	for _, p := range others {
		_ = p.sess.send(proto.TypePeerCandidate, mustJSON(proto.PeerCandidate{
			Version:   proto.ProtocolVersion,
			PeerMAC:   selfMAC,
			PeerIP:    self.ip.String(),
			PeerName:  self.name,
			Candidate: candidate,
		}))
		s.logger.Printf("vrndis server: 向 %s 推送新候选 %s -> %s", p.mac, selfMAC, candidate)
	}
}

// distributeCandidates 向当前 session 的对端下发 peer 候选。
// 若对端已上线且已上报候选，立即下发；否则等待对端上线后再下发。
func (s *Server) distributeCandidates(sess *session, selfMAC string) {
	s.peersMu.RLock()
	peers := make([]*peerInfo, 0, len(s.peers))
	for _, p := range s.peers {
		if p.mac != selfMAC && p.candidate != "" {
			peers = append(peers, p)
		}
	}
	s.peersMu.RUnlock()

	for _, p := range peers {
		_ = sess.send(proto.TypePeerCandidate, mustJSON(proto.PeerCandidate{
			Version:   proto.ProtocolVersion,
			PeerMAC:   p.mac,
			Candidate: p.candidate,
		}))
		s.logger.Printf("vrndis server: 向 %s 下发对端 %s 候选 %s", selfMAC, p.mac, p.candidate)
	}
}

// answerPeerQuery 应答 PeerQuery：按 target（IP/MAC，空=全部）在 peers
// 表中查找已上报候选的 peer 并逐条回发；无匹配时回一条空 Candidate 便于客户端收尾。
func (s *Server) answerPeerQuery(sess *session, target string) {
	s.peersMu.RLock()
	var matched []*peerInfo
	for _, p := range s.peers {
		if p.candidate == "" {
			continue // 未上报候选（未开 P2P / 打洞未成功）不应答
		}
		if target == "" || p.ip.String() == target || p.mac == target || p.name == target {
			matched = append(matched, p)
		}
	}
	s.peersMu.RUnlock()
	if len(matched) == 0 {
		_ = sess.send(proto.TypePeerCandidate, mustJSON(proto.PeerCandidate{
			Version: proto.ProtocolVersion,
		}))
		return
	}
	for _, p := range matched {
		_ = sess.send(proto.TypePeerCandidate, mustJSON(proto.PeerCandidate{
			Version:   proto.ProtocolVersion,
			PeerMAC:   p.mac,
			PeerIP:    p.ip.String(),
			PeerName:  p.name,
			Candidate: p.candidate,
		}))
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
