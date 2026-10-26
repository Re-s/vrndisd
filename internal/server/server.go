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

	"github.com/Re-s/vrndisd/internal/addr"
	"github.com/Re-s/vrndisd/internal/crypto"
	"github.com/Re-s/vrndisd/internal/ethernet"
	"github.com/Re-s/vrndisd/internal/ethswitch"
	"github.com/Re-s/vrndisd/internal/loglevel"
	"github.com/Re-s/vrndisd/internal/mgmt"
	"github.com/Re-s/vrndisd/internal/proto"
	"github.com/Re-s/vrndisd/internal/tap"
)

const tapPort = "tap"

// 服务端版本号
var Version = "dev"

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

	// ManagementListen 本机管理监听地址（clients/status 命令实时查询，
	// 不走客户端协议、无需 PSK）。为空时用默认 127.0.0.1:7790；"-" 表示禁用。
	ManagementListen string

	// 日志级别：debug/info/warn/error（默认 info）。
	LogLevel string

	// LeaseFile 租约簿路径：按客户端 name 持久化 IP/MAC，服务器重启后
	// 同一客户端重连复用原租约，MAC 不随连接顺序漂移。为空时禁用。
	LeaseFile string
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
	mac        string // 虚拟 MAC（同时也是 peers 表键）
	ip         net.IP
	name       string
	candidate  string   // 主候选（首个公网候选），兼容旧客户端的单字段下发
	candidates []string // 全部候选：srflx / UPnP 映射 / LAN host / 公网 v6
	egressIP   net.IP   // 该 peer 的公网出口（首个公网 v4 候选的 host）
	sess       *session // 推送候选用（复用带锁的 send）
}

// Server 虚拟 RNDIS 服务器。
type Server struct {
	cfg    Config
	dev    tap.Device
	pool   *addr.Pool
	leases *leaseBook
	sw     *ethswitch.LearningSwitch
	tapMAC net.HardwareAddr
	ln     net.Listener
	logger *loglevel.Logger

	// 背压/失败统计：写入 TAP 失败是逐帧事件，合并为每秒一条统计输出。
	tapFailures atomic.Uint64
	tapFailLog  *loglevel.RateLimiter
	mgmtSrv     *mgmt.Server // 本机管理 TCP（clients/status 查询，端口自动避让）

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

// controlWriteTimeout 控制面/管理面同步写的超时上限。
// 对端半死（接收窗口关闭且无 RST）时 TCP 写会无限阻塞，曾经把
// handleConn 的消息处理 goroutine 冻结并雪崩至全会话（2026-10-19 事故）。
// 10s 仍写不出去即判为死连接。
const controlWriteTimeout = 10 * time.Second

type session struct {
	id     uint64
	conn   net.Conn
	peer   string
	lease  *addr.Lease
	macKey string
	name   string
	// P2P 状态（管理面展示用）
	p2pCandidate  string   // 主候选（"" 表示未上报）
	p2pCandidates []string // 全部候选（多路化：srflx/LAN/v6）
	p2pReady      bool     // 客户端通知打洞成功
	p2pPeerMAC    string   // 打通的对端虚拟 MAC
	p2pMu         sync.RWMutex
	writeMu       sync.Mutex
	outQ          chan []byte // 有界发送队列：慢消费者只丢自己的帧，不阻塞交换机
	done          chan struct{}
	dropped       atomic.Uint64
	crypto        *crypto.SecureConn // 加密通道（PSK 握手后非 nil）
}

// send 同步发送一条消息（写超时 controlWriteTimeout）。调用方通常在
// 回应该连接自身消息的路径上，阻塞上限即本连接的写超时。
func (s *session) send(typ byte, body []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.writeMsg(typ, body)
}

// writeMsg 加密并写出一条消息。调用者必须持有 writeMu。
func (s *session) writeMsg(typ byte, body []byte) error {
	if s.crypto != nil {
		// 信封模式：[type]+body 整体加密，外层统一 TypeData
		plaintext := append([]byte{typ}, body...)
		encrypted, err := s.crypto.Encrypt(plaintext)
		if err != nil {
			return err
		}
		typ, body = proto.TypeData, encrypted
	}
	// 每次写前重设：既封顶本次阻塞，又不留残留超时状态
	_ = s.conn.SetWriteDeadline(time.Now().Add(controlWriteTimeout))
	return proto.WriteMessage(s.conn, typ, body)
}

// sendControl 异步发送控制消息（跨连接通知：候选推送等）。
// 收件人半死时同步写会冻结发件人 handleConn 的消息循环并雪崩，
// 异步化为各自 writeMu + 写超时，发件人零等待。
func (s *session) sendControl(typ byte, body []byte) {
	go func() {
		_ = s.send(typ, body)
	}()
}

// p2pReadyPeer 返回该会话是否已与对端建立 P2P 直连及对端虚拟 MAC。
// 候选推送据此跳过已直连的双方——隧道活着时新候选无意义，且客户端
// 消费端不取会积满丢弃刷日志。状态由 P2PReady 置位、P2PWithdraw
// 或连接断开复位，与客户端隧道生命周期一致。
func (s *session) p2pReadyPeer() (bool, string) {
	s.p2pMu.RLock()
	defer s.p2pMu.RUnlock()
	return s.p2pReady, s.p2pPeerMAC
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
//
// writeMsg 非原子（5 字节头与 body 分两次写），必须持 writeMu 与
// send/sendControl（控制消息、候选推送等）互斥，否则字节流交错、
// 对端解密出乱序 seq -> "replayed message" 误断连（高负载下必发）。
func (s *session) writer() {
	for {
		select {
		case frame := <-s.outQ:
			s.writeMu.Lock()
			err := s.writeMsg(proto.TypeData, frame)
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
	level, lerr := loglevel.ParseLevel(cfg.LogLevel)
	if lerr != nil {
		// 非法级别不阻断启动：回退 info 并提示。
		log.Printf("vrndis server: %v，回退 info", lerr)
		level = loglevel.LevelInfo
	}
	return &Server{
		cfg:        cfg,
		pool:       pool,
		leases:     loadLeaseBook(cfg.LeaseFile),
		sw:         ethswitch.New(),
		tapMAC:     tapMAC,
		ln:         ln,
		logger:     loglevel.New(log.Default(), level),
		tapFailLog: loglevel.NewRateLimiter(time.Second),
		sessions:   make(map[string]*session),
		peers:      make(map[string]*peerInfo),
		stopCh:     make(chan struct{}),
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
	s.logger.Infof("vrndis server: TAP %s 就绪（网关 %s/%d），监听 %s",
		dev.Name(), s.cfg.Gateway, s.pool.Prefix(), s.ln.Addr())

	s.mgmtSrv = mgmt.Serve(s.cfg.ManagementListen, s.managementState, s.logger.Logger)
	if a := s.mgmtSrv.Addr(); a != "" {
		s.logger.Infof("vrndis server: 本机管理端口 %s", a)
	}

	s.wg.Add(3)
	go s.acceptLoop()
	go s.tapLoop()
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
		if s.mgmtSrv != nil {
			s.mgmtSrv.Close()
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

// defaultManagementListen 平台默认管理监听地址（仅本机回环，避免外网暴露）。
func defaultManagementListen() string {
	return "127.0.0.1:7790"
}

// managementListenAddr 解析管理监听地址（"-" 禁用返回空）。
func (s *Server) managementListenAddr() string {
	switch {
	case s.cfg.ManagementListen == "-":
		return ""
	case s.cfg.ManagementListen != "":
		return s.cfg.ManagementListen
	default:
		return defaultManagementListen()
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
			n := len(sess.p2pCandidates)
			if n > 1 {
				p2p = fmt.Sprintf("stun-ok %s（打洞中，%d 候选）", cand, n)
			} else {
				p2p = "stun-ok " + cand + "（打洞中）"
			}
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
		Role:      "server",
		Clients:   list,
	}
}

// ManagementAddr 返回当前管理监听地址（未监听/已停止/已禁用返回空）。
func (s *Server) ManagementAddr() string {
	if s.mgmtSrv == nil {
		return ""
	}
	return s.mgmtSrv.Addr()
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
		sess.p2pMu.RLock()
		name := sess.name
		sess.p2pMu.RUnlock()
		clients = append(clients, &ClientInfo{
			IP:        lease.IP,
			MAC:       lease.MAC,
			Peer:      sess.peer,
			Name:      name,
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
			n := len(sess.p2pCandidates)
			if n > 1 {
				p2p = fmt.Sprintf("stun-ok %s（打洞中，%d 候选）", cand, n)
			} else {
				p2p = "stun-ok " + cand + "（打洞中）"
			}
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

// allocateLease 为新连接分配租约：name 非空且租约簿有记录时优先复用原
// IP/MAC（跨服务器重启稳定）；记录的地址已被占用或无记录时回退顺序分配，
// 后者同时登记/更新租约簿。分配失败时返回 nil（调用方应关闭连接）。
func (s *Server) allocateLease(name, peer string) *addr.Lease {
	if name != "" {
		if ip, mac, ok := s.leases.lookup(name); ok {
			if l, err := s.pool.AllocateSpecific(ip, mac); err == nil {
				s.logger.Infof("vrndis server: 复用租约 %s -> %s (%s)", name, l.IP, l.MAC)
				return l
			}
			// 记录的地址被占用（同名冲突/漂移）：顺序分配，
			// 记录随后更新到该 name 的当前实际租约。
		}
	}
	l, err := s.pool.Allocate()
	if err != nil {
		s.logger.Warnf("vrndis server: 无法为 %s 分配地址: %v", peer, err)
		return nil
	}
	if name != "" {
		s.leases.record(name, l)
	}
	return l
}

func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.stopCh:
				return
			default:
				s.logger.Warnf("vrndis server: accept: %v", err)
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
	// 兜底防御：任何消息处理路径的 panic 都只死本连接，绝不拖垮服务器
	// （2026-10-19 事故：一次空指针 panic 在持锁区间发生，进程没崩但
	// 服务器被冻结到只能 SIGKILL）。recover 在清理 defer 之前注册，
	// panic 时清理 defer 先跑（关 writer、回收租约），再由本 recover 吞掉。
	defer func() {
		if r := recover(); r != nil {
			conn.Close()
			s.logger.Errorf("vrndis server: 客户端 %s 处理消息时 panic，已关闭该连接: %v", peer, r)
		}
	}()
	var sec *crypto.SecureConn
	if len(s.cfg.PSK) > 0 {
		var err error
		sec, err = crypto.ServerHandshake(conn, s.cfg.PSK, proto.ProtocolVersion)
		if err != nil {
			s.logger.Warnf("vrndis server: 加密握手失败 %s: %v", peer, err)
			conn.Close()
			return
		}
		s.logger.Infof("vrndis server: 加密连接建立 %s", peer)
	}
	// 先读首条消息拿到客户端 name，再分配地址：客户端建立后立即发 Hello，
	// 此处不改变协议，仅把读取时机提前以支持按 name 复用原租约。
	// 首条消息带 15s 截点：从不发数据的对端不应永久占用读取 goroutine。
	conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	firstMsg, err := proto.ReadMessage(conn)
	conn.SetReadDeadline(time.Time{})
	if err != nil {
		s.logger.Infof("vrndis server: 等待 %s 的首条消息失败: %v", peer, err)
		conn.Close()
		return
	}
	if sec != nil && len(firstMsg.Body) > 0 {
		plain, decErr := sec.Decrypt(firstMsg.Body)
		if decErr != nil {
			s.logger.Errorf("vrndis server: 解密失败 %s: %v", peer, decErr)
			conn.Close()
			return
		}
		if len(plain) == 0 {
			s.logger.Errorf("vrndis server: 解密得到空数据 %s", peer)
			conn.Close()
			return
		}
		firstMsg.Type = plain[0]
		firstMsg.Body = plain[1:]
	}
	var firstName string
	if firstMsg.Type == proto.TypeHello {
		var hello proto.Hello
		if json.Unmarshal(firstMsg.Body, &hello) == nil {
			firstName = hello.Name
		}
	}
	lease := s.allocateLease(firstName, peer)
	if lease == nil {
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
		s.logger.Infof("vrndis server: 客户端离开 %s，回收 %s（丢帧 %d）",
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
		s.logger.Warnf("vrndis server: 下发地址失败 %s: %v", peer, err)
		return
	}
	s.logger.Infof("vrndis server: 客户端接入 %s -> 分配 %s (%s)", peer, lease.IP, lease.MAC)

	first := true // 首条消息已在分配租约前读出，先处理它再继续读流
	for {
		var msg proto.Message
		if first {
			msg = firstMsg
			first = false
		} else {
			var rerr error
			msg, rerr = proto.ReadMessage(conn)
			if rerr != nil {
				s.logger.Infof("vrndis server: 客户端 %s 断开: %v", peer, rerr)
				return
			}
		}
		if sess.crypto != nil && len(msg.Body) > 0 {
			plain, decErr := sess.crypto.Decrypt(msg.Body)
			if decErr != nil {
				s.logger.Errorf("vrndis server: 解密失败 %s: %v", peer, decErr)
				return
			}
			if len(plain) == 0 {
				s.logger.Errorf("vrndis server: 解密得到空数据 %s", peer)
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
				sess.p2pMu.Lock()
				sess.name = hello.Name
				sess.p2pMu.Unlock()
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
			if adv.MAC == "" {
				break
			}
			// 旧客户端只有单 Candidate 字段
			list := adv.Candidates
			if len(list) == 0 {
				if adv.Candidate == "" {
					break
				}
				list = []string{adv.Candidate}
			}
			s.registerPeer(sess, sess.lease.MAC.String(), adv.Candidate, list)
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
			sess.p2pCandidates = nil
			sess.p2pReady = false
			sess.p2pPeerMAC = ""
			sess.p2pMu.Unlock()
			s.logger.Infof("vrndis server: peer %s 撤销候选（回退中继）", mac)
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
			s.logger.Infof("vrndis server: peer %s P2P 打洞成功 -> %s",
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
	s.logger.Infof("vrndis server: 注销 peer %s", mac)
}

// candidateHost 从 "host:port" 候选串中取出 host 部分。
func candidateHost(candidate string) string {
	host, _, err := net.SplitHostPort(candidate)
	if err != nil {
		return ""
	}
	return host
}

func (s *Server) registerPeer(sess *session, mac, candidate string, candidates []string) {
	// 拒绝隧道网段候选：客户端 STUN 请求若被路由进隧道，服务器看到的源是
	// 隧道内网地址（如 10.77.0.6:33395）。该端口只是 STUN 反射的源端口，
	// 对端本机并未监听；向隧道地址发包只能绕中继且必然丢弃，是挤占全网
	// 打洞预算的废候选。LAN 地址（192.168.x 等）不在此列——同 NAT 设备
	// 可借 LAN 候选内网直连，绕开 hairpin 缺失。
	// 多候选下逐条剔除隧道地址，保留其它有效候选（单条污染不再整体拒绝）。
	filtered := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if host := candidateHost(c); host != "" {
			if ip := net.ParseIP(host); ip != nil && s.pool.Contains(ip) {
				s.logger.Warnf("vrndis server: 拒绝隧道地址候选 %s -> %s", mac, c)
				continue
			}
		}
		filtered = append(filtered, c)
	}
	if len(filtered) == 0 {
		return
	}
	// 主候选 = 首个公网候选（兼容旧客户端单字段下发），无公网候选时回退首个。
	primary := ""
	for _, c := range filtered {
		if isPublicCandidate(c) {
			primary = c
			break
		}
	}
	if primary == "" {
		primary = filtered[0]
	}
	s.peersMu.Lock()
	s.peers[mac] = &peerInfo{
		mac:        mac,
		ip:         sess.lease.IP,
		name:       sess.name,
		candidate:  primary,
		candidates: filtered,
		egressIP:   candidateEgress(filtered),
		sess:       sess,
	}
	sess.p2pMu.Lock()
	sess.p2pCandidate = primary
	sess.p2pCandidates = filtered
	sess.p2pMu.Unlock()
	s.peersMu.Unlock()
	s.logger.Infof("vrndis server: 候选注册 %s -> %v（出口 %v）", mac, filtered, candidateEgress(filtered))
	// 向其它已上报候选的在线 peer 推送本端新候选。
	// 客户端两端上线时序错开时，只有此处推送才能让对端在打洞窗口内
	// 拿到本端候选（distributeCandidates 只在新 session 上线时下发）。
	s.pushCandidateToOthers(sess, mac, filtered)
}

// isPublicCandidate 判断候选是否公网可路由（跨 NAT 下发安全）：
// 公网 v4 或 global v6（非 ULA/link-local/loopback）。私网 v4 与 ULA v6
// 只能作为同 NAT 内网直连候选，不得下发给不同出口的 peer
// （防空打洞 + 防内网地址外泄给陌生人）。
// 注意：Go std 的 IsPrivate 不含 CGNAT 100.64/10（RFC6598），需单独排除。
func isPublicCandidate(c string) bool {
	host, _, err := net.SplitHostPort(c)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	if ip.To4() != nil && isCGNAT(ip) {
		return false
	}
	return !ip.IsPrivate() // RFC1918(v4) 与 ULA(fc00::/7, v6)
}

// isCGNAT 判断 IPv4 是否落在 RFC6598 运营商级 NAT 网段 100.64.0.0/10。
func isCGNAT(ip net.IP) bool {
	if ip4 := ip.To4(); ip4 != nil {
		return ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127
	}
	return false
}

// candidateEgress 取候选列表的公网出口 IP（首个公网 v4 候选的 host）。
// 用于内网候选下发过滤：同出口的 peer 才互发 LAN 候选。
func candidateEgress(cands []string) net.IP {
	for _, c := range cands {
		host, _, err := net.SplitHostPort(c)
		if err != nil {
			continue
		}
		ip := net.ParseIP(host)
		if ip == nil {
			continue
		}
		if v4 := ip.To4(); v4 != nil && !v4.IsPrivate() && !v4.IsLoopback() && !v4.IsLinkLocalUnicast() && !isCGNAT(v4) {
			return v4
		}
	}
	return nil
}

// candidatesFor 按接收方过滤提供者的候选后返回：
//   - 公网候选：全量下发（对端并发尝试，先通者胜）
//   - 内网候选（私网 v4/ULA v6 host 候选）：仅当接收方与提供者同公网出口
//     （同 NAT/LAN）时下发。无公网候选的一方无法判定亲缘，保守不下发。
func candidatesFor(receiver, provider *peerInfo) []string {
	out := make([]string, 0, len(provider.candidates))
	for _, c := range provider.candidates {
		if isPublicCandidate(c) {
			out = append(out, c)
			continue
		}
		// receiver 可能为 nil（查询者未上报候选）：保守判为未知出口，只回公网候选。
		// 空指针解引用曾致整服务器卡死（2026-10-19 事故），此处必须先判 nil。
		if provider.egressIP != nil && receiver != nil && receiver.egressIP != nil && receiver.egressIP.Equal(provider.egressIP) {
			out = append(out, c)
		}
	}
	return out
}

// containsStr 判断字符串切片是否包含某值（无 slices 依赖的最小实现）。
func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// pushCandidateToOthers 把新注册的候选推送给其它已在线且已上报候选的 peer。
func (s *Server) pushCandidateToOthers(sess *session, selfMAC string, selfCands []string) {
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
		if ready, peerMAC := p.sess.p2pReadyPeer(); ready && peerMAC == selfMAC {
			continue // 接收方已与 self 直连，无需再推
		}
		list := candidatesFor(p, self)
		if len(list) == 0 {
			continue
		}
		p.sess.sendControl(proto.TypePeerCandidate, mustJSON(proto.PeerCandidate{
			Version:    proto.ProtocolVersion,
			PeerMAC:    selfMAC,
			PeerIP:     self.ip.String(),
			PeerName:   self.name,
			Candidate:  list[0],
			Candidates: list,
		}))
		s.logger.Infof("vrndis server: 向 %s 推送新候选 %s -> %v", p.mac, selfMAC, list)
	}
}

// distributeCandidates 向当前 session 的对端下发 peer 候选。
// 若对端已上线且已上报候选，立即下发；否则等待对端上线后再下发。
func (s *Server) distributeCandidates(sess *session, selfMAC string) {
	s.peersMu.RLock()
	peers := make([]*peerInfo, 0, len(s.peers))
	receiver := s.peers[selfMAC] // 同锁内取：s.peers 并发注册/注销时无锁读会 race
	if receiver == nil {
		// 接收方自己尚未注册候选（本函数由刚上报者触发，理论不至此），
		// 只下发公网候选，避免把对端内网地址泄露给未知出口。
		receiver = &peerInfo{}
	}
	for _, p := range s.peers {
		if p.mac != selfMAC && p.candidate != "" {
			peers = append(peers, p)
		}
	}
	s.peersMu.RUnlock()

	for _, p := range peers {
		if ready, peerMAC := sess.p2pReadyPeer(); ready && peerMAC == p.mac {
			continue // self 已与该 peer 直连，无需其候选
		}
		list := candidatesFor(receiver, p)
		if len(list) == 0 {
			continue
		}
		_ = sess.send(proto.TypePeerCandidate, mustJSON(proto.PeerCandidate{
			Version:    proto.ProtocolVersion,
			PeerMAC:    p.mac,
			Candidate:  list[0],
			Candidates: list,
		}))
		s.logger.Infof("vrndis server: 向 %s 下发对端 %s 候选 %v", selfMAC, p.mac, list)
	}
}

// answerPeerQuery 应答 PeerQuery：按 target（IP/MAC/name，空=全部）列出
// 在线设备。与 ping/peers 诊断命令的"看不到设备"根因修复配合：此前只在
// peers 表中回发"此刻持有候选"的条目，而候选在打洞失败后被撤回退避，
// 诊断瞬间几乎必落空——peers 永远空而 clients 有设备。
//
// 现按 sessions 全量应答：有候选的带上候选地址（供 disco 探测/打洞），
// 无候选的作为 relay-only 条目下发（PeerMAC 非空、Candidate 为空），
// 最后回一条全空 Candidate 作为列表结束标记。常驻客户端对空候选的
// 处理是 ResolveUDPAddr 失败即丢弃（见 client.go TypePeerCandidate 分支），
// 不参与打洞，兼容无副作用。
func (s *Server) answerPeerQuery(sess *session, target string) {
	selfMAC := sess.lease.MAC.String()
	// 接收方自己的 peer 记录：未上报候选时为 nil，保守按"未知出口"处理
	// （内网候选只给同出口 peer，未知则只回公网候选）。
	receiver := s.peers[selfMAC]
	if receiver == nil {
		receiver = &peerInfo{}
	}
	type peerEntry struct {
		mac, ip, name, cand string
		list                []string
	}
	// 收集阶段：锁在闭包内以 defer 释放——即便低概率 panic 也不能把
	// s.mu/peersMu 的读锁带进坟墓（2026-10-19 卡死事故根因：panic 跳过
	// 闭包尾部的显式 RUnlock，两把读锁被冻结 goroutine 永久持有，新会话
	// 注册的 Lock 排队死等，管理面/数据面/停机信号全部被拖死到 SIGKILL）。
	// 应答发送刻意留在锁外：慢查询者最多耽搁自己一条连接。
	collect := func() (matched []peerEntry) {
		s.mu.RLock()
		defer s.mu.RUnlock()
		s.peersMu.RLock()
		defer s.peersMu.RUnlock()
		for _, ses := range s.sessions {
			mac := ses.lease.MAC.String()
			if mac == selfMAC {
				continue // 不回发查询者自己
			}
			ses.p2pMu.RLock()
			name := ses.name
			ses.p2pMu.RUnlock()
			if target != "" && ses.lease.IP.String() != target && mac != target && name != target {
				continue
			}
			cand := ""
			var list []string
			if p := s.peers[mac]; p != nil {
				// 与推送/distribute 同一套过滤：内网候选只给同出口 peer。
				// PeerQuery 由客户端 p2pConnect 拉取历史候选直接喂打洞，
				// 不过滤会同 push 一样外泄内网地址/制造空打洞。
				list = candidatesFor(receiver, p)
				cand = p.candidate
				if len(list) > 0 && !containsStr(list, cand) {
					cand = list[0] // 主候选被过滤时回退到过滤后首条
				}
				if len(list) == 0 {
					cand = "" // 全部候选对该接收方不可见 = 无候选
				}
			}
			matched = append(matched, peerEntry{mac: mac, ip: ses.lease.IP.String(), name: name, cand: cand, list: list})
		}
		return matched
	}
	matched := collect()
	for _, e := range matched {
		_ = sess.send(proto.TypePeerCandidate, mustJSON(proto.PeerCandidate{
			Version:    proto.ProtocolVersion,
			PeerMAC:    e.mac,
			PeerIP:     e.ip,
			PeerName:   e.name,
			Candidate:  e.cand,
			Candidates: e.list,
		}))
	}
	// 列表结束标记（全空）：匹配为空时第一条即此标记，令客户端立即收尾。
	_ = sess.send(proto.TypePeerCandidate, mustJSON(proto.PeerCandidate{
		Version: proto.ProtocolVersion,
	}))
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
				s.logger.Errorf("vrndis server: TAP 读取失败: %v", err)
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
					// 逐帧事件会刷屏：计数合并为每秒一条。
					n := s.tapFailures.Add(1)
					if s.tapFailLog.Allow("tap-write") {
						s.logger.Errorf("vrndis server: 写入 TAP 失败（过去 1s 共 %d 帧）: %v", n, err)
						s.tapFailures.Store(0)
					}
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
