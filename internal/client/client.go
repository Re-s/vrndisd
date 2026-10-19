// Package client 实现虚拟 RNDIS 客户端。
//
// 连接服务器（TCP）→ 收 ASSIGN（服务器分配的地址）→ 配置本地虚拟网卡 →
// 双向搬运以太网帧。对操作系统而言本地网卡就是一块"插上就联网"的网卡。
//
// 自愈（断网自动重连）：
//   - 单个连接的生命周期与客户端整体生命周期分离：连接断开只销毁该连接，
//     不关闭共享的虚拟网卡；重连成功后按新的 ASSIGN 重新刷新网卡 IP/MAC。
//   - 主动断线检测：客户端周期性发送 TypePing，收到 TypePong 记为活跃；
//     心跳超时（或读连接返回 EOF/错误）即判定断线并触发重连。
//   - 重连采用指数退避，退避基数与上限可配置；重连成功后退避重置。
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"sync"
	"time"

	"vrndis/internal/ethernet"
	"vrndis/internal/p2p"
	"vrndis/internal/proto"
	"vrndis/internal/tap"

	"vrndis/internal/crypto"
)

// 默认心跳参数。
const (
	defaultHeartbeatInterval = 10 * time.Second
	defaultHeartbeatTimeout  = 30 * time.Second
	defaultReconnectBackoff  = 1 * time.Second
	defaultMaxReconnectBoff  = 30 * time.Second
)

// Config 客户端配置。
type Config struct {
	Server       string        // 服务器地址 host:port
	TapName      string        // 本地虚拟网卡名
	Name         string        // 客户端名称（日志用途）
	DefaultRoute bool          // 把系统默认路由指向虚拟网卡
	DialTimeout  time.Duration // 建连超时
	P2P          bool          // 是否开启 P2P 直连（默认关闭，纯中继）
	STUNServer   string        // STUN 服务器地址，P2P 时使用
	Device       tap.Device    // 可选：注入设备（测试用）

	// 自愈（断网自动重连）。
	Reconnect           bool          // 断线后自动重连（默认 false；cmd 默认 true）
	MaxRetries          int           // 最大连续重连次数，0 = 无限（默认 0）
	ReconnectBackoff    time.Duration // 重连退避基数（默认 1s）
	ReconnectMaxBackoff time.Duration // 重连退避上限（默认 30s）

	// 主动心跳（客户端 Ping → 服务器 Pong）。
	HeartbeatInterval time.Duration // 心跳发送间隔，0 = 默认 10s
	HeartbeatTimeout  time.Duration // 心跳超时，0 = 默认 30s

	// 加密。
	PSK []byte // 预共享密钥；非 nil 时与服务器握手加密
}

// Client 虚拟 RNDIS 客户端。
type Client struct {
	cfg    Config
	dev    tap.Device
	logger *log.Logger

	// 共享数据面：devToSock 长期驻留，跨连接复用同一块虚拟网卡。
	devPumpOnce sync.Once

	// 当前连接状态（重连时会更换）。
	connMu   sync.Mutex
	conn     net.Conn
	connDone chan struct{} // 当前连接生命周期结束信号
	connOnce sync.Once     // 保证 connDone 只关闭一次
	connWG   sync.WaitGroup
	writeMu  sync.Mutex
	crypto   *crypto.SecureConn

	assignMu sync.RWMutex
	assign   *proto.Assignment

	lastPongMu sync.Mutex
	lastPong   time.Time // 最近一次收到 Pong/Ping 的时间（心跳保活）

	// P2P 运行状态
	p2pMu       sync.Mutex
	tunnel      *p2p.Tunnel      // 当前 P2P 隧道（nil = 未打通/已回退）
	peerMAC     net.HardwareAddr // 对端虚拟 MAC
	peering     bool             // 是否正在打洞中
	peerCandCh  chan *peerInfo   // 服务器下发的对端候选（仅 P2P）
	p2pActive   bool             // p2pConnect 运行中标记（事件驱动重启去抖）
	p2pFailures int              // 连续打洞失败轮数（成功清零）
	p2pBackoff  time.Time        // 退避结束时刻；早于此后不启动新一轮打洞

	stopCh    chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

// New 创建客户端（未连接）。
func New(cfg Config) *Client {
	if cfg.TapName == "" {
		cfg.TapName = "vrndis-c0"
	}
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = 10 * time.Second
	}
	return &Client{
		cfg:        cfg,
		logger:     log.Default(),
		stopCh:     make(chan struct{}),
		peerCandCh: make(chan *peerInfo, 16),
	}
}

// Connect 建立一次连接、接收地址分配、配置本地网卡并启动搬运协程。
//
// 与 Run 不同：Connect 只建立一个连接周期，不负责重连。返回本次分配结果。
func (c *Client) Connect() (*proto.Assignment, error) {
	return c.connectOnce()
}

// connectOnce 完成一次连接周期：建连 → Hello → Assign → 配置网卡 → 启动协程。
// 网卡（真实 TAP 或注入的 Fake）首次创建后跨连接复用，重连时仅刷新 IP/MAC。
func (c *Client) connectOnce() (*proto.Assignment, error) {
	conn, err := net.DialTimeout("tcp", c.cfg.Server, c.cfg.DialTimeout)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", c.cfg.Server, err)
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}

	// 加密握手（可选）：如果客户端配置了 PSK，则必须先完成加密握手。
	if len(c.cfg.PSK) > 0 {
		secure, err := crypto.ClientHandshake(conn, c.cfg.PSK, proto.ProtocolVersion)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("握手失败（服务器可能未启用加密，双方 --psk 需一致）: %w", err)
		}
		c.crypto = secure
		c.logger.Printf("vrndis client: 加密握手成功")
	}

	helloBody, _ := json.Marshal(proto.Hello{Version: proto.ProtocolVersion, Name: c.cfg.Name})
	// Hello 也走加密信封（c.send 内部处理 crypto 逻辑）
	if err := c.send(conn, proto.TypeHello, helloBody); err != nil {
		conn.Close()
		return nil, err
	}
	msg, err := proto.ReadMessage(conn)
	if err != nil {
		conn.Close()
		if len(c.cfg.PSK) == 0 {
			// 本机未加密，对端却在握手阶段就关了连接：最常见的原因是
			// 服务器配了 --psk 而客户端没带。
			return nil, fmt.Errorf("read assignment: %w（提示: 本机未配置 --psk，服务器可能要求加密握手）", err)
		}
		return nil, fmt.Errorf("read assignment: %w", err)
	}
	if c.crypto != nil {
		plaintext, err := c.crypto.Decrypt(msg.Body)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("解密 ASSIGN: %w", err)
		}
		msg.Type = plaintext[0]
		msg.Body = plaintext[1:]
	}
	if msg.Type != proto.TypeAssign {
		conn.Close()
		return nil, fmt.Errorf("期望 ASSIGN，收到 %s", proto.TypeName(msg.Type))
	}
	var assign proto.Assignment
	if err := json.Unmarshal(msg.Body, &assign); err != nil {
		conn.Close()
		return nil, fmt.Errorf("解析 ASSIGN 失败: %w", err)
	}
	// 服务器网卡的 MAC（Assignment.ServerMAC），
	// 作为上行伪以太网帧发往网关时的目的 MAC 注入。
	serverMAC, err := ethernet.MACFromString(assign.ServerMAC)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("ASSIGN 中的 ServerMAC 无效: %w", err)
	}

	// 网卡：注入的 Fake 或首次打开的真实 TAP；重连复用同一块网卡。
	dev := c.cfg.Device
	if dev == nil && c.dev == nil {
		opened, err := tap.Open(c.cfg.TapName)
		if err != nil {
			conn.Close()
			return nil, err
		}
		c.dev = opened
		dev = opened
	} else if dev == nil {
		dev = c.dev
	}
	c.dev = dev // 统一登记共享网卡（注入 Fake 或真实 TAP）
	mac, err := ethernet.MACFromString(assign.MAC)
	if err != nil {
		conn.Close()
		return nil, err
	}
	ip := net.ParseIP(assign.IP).To4()
	if ip == nil {
		conn.Close()
		return nil, fmt.Errorf("ASSIGN 中的 IP 无效: %q", assign.IP)
	}
	if err := dev.SetMAC(mac); err != nil {
		conn.Close()
		return nil, fmt.Errorf("set mac: %w", err)
	}
	if err := dev.SetIPv4(ip, assign.Prefix); err != nil {
		conn.Close()
		return nil, fmt.Errorf("set ipv4: %w", err)
	}
	if err := dev.Up(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("tap up: %w", err)
	}
	if c.cfg.DefaultRoute {
		gw := net.ParseIP(assign.Gateway)
		// 默认路由必须排除服务器端点：否则去服务器公网 IP 的包（含隧道
		// TCP 连接自身的 ACK/心跳）会被劫持进隧道，而隧道网关的 ARP
		// 应答又依赖这条连接 → 死锁 → "Destination Host Unreachable"。
		var exclude net.IP
		if host, _, err := net.SplitHostPort(c.cfg.Server); err == nil {
			exclude = net.ParseIP(host)
		}
		if rp, ok := dev.(interface {
			AddDefaultRouteProtected(gateway, exclude net.IP) error
		}); ok && exclude != nil {
			if err := rp.AddDefaultRouteProtected(gw, exclude); err != nil {
				c.logger.Printf("vrndis client: 设置默认路由失败（继续运行）: %v", err)
			}
		} else if err := dev.AddDefaultRoute(gw); err != nil {
			c.logger.Printf("vrndis client: 设置默认路由失败（继续运行）: %v", err)
		}
	}

	// 方案 B（L2↔L3 适配）：把 Assignment 的对端信息注入网卡。
	// 纯二层网卡（Linux/Android TAP）此为空操作；
	// wintun（Windows 三层 TUN）用它完成裸 IP 包 ↔ 以太网帧的
	// 转换并自答发往本机的 ARP 请求（详见 internal/tap/l2l3）。
	// 网关 IP/MAC 分开注入：二者来自 Assignment 的不同字段。
	if err := dev.SetPeerInfo(mac, serverMAC, ip); err != nil {
		conn.Close()
		return nil, fmt.Errorf("set peer info: %w", err)
	}
	if gw := net.ParseIP(assign.Gateway); gw != nil {
		if s, ok := dev.(interface{ SetGatewayIP(net.IP) }); ok {
			s.SetGatewayIP(gw)
		}
	}
	// ARP 回传通道：网卡自答的 ARP Reply 帧不经过 wintun，
	// 经此回调写回 TCP 中继，让服务器内核解析到本机 MAC。
	// conn 在闭包中被并发读取（仅 WriteMessage），由 connMu 保护。
	if s, ok := dev.(interface{ SetARPInject(func(frame []byte)) }); ok {
		s.SetARPInject(func(frame []byte) {
			c.connMu.Lock()
			conn := c.conn
			c.connMu.Unlock()
			if conn == nil {
				return // 未连接，丢弃
			}
			if c.crypto != nil {
				plaintext := append([]byte{proto.TypeData}, frame...)
				var err error
				frame, err = c.crypto.Encrypt(plaintext)
				if err != nil {
					return
				}
			}
			c.writeMu.Lock()
			err := proto.WriteMessage(conn, proto.TypeData, frame)
			c.writeMu.Unlock()
			if err != nil {
				c.connMu.Lock()
				isCurrent := c.conn == conn
				c.connMu.Unlock()
				if isCurrent {
					c.logger.Printf("vrndis client: ARP 回传失败: %v", err)
				}
			}
		})
	}

	c.assignMu.Lock()
	c.assign = &assign
	c.assignMu.Unlock()

	// 登记当前连接并启动该连接的协程（数据面 devToSock 为长期驻留，只启动一次）。
	c.connMu.Lock()
	c.conn = conn
	c.connDone = make(chan struct{})
	c.connOnce = sync.Once{}
	c.connMu.Unlock()

	c.connWG.Add(2)
	go c.sockToDev(conn)
	go c.heartbeat(conn)

	c.devPumpOnce.Do(func() {
		c.wg.Add(1)
		go c.devToSock()
	})

	// P2P：仅在本轮新连接且尚未有活跃隧道时启动打洞。
	// TCP 中继始终常驻（devToSock 长期驻留），P2P 只是数据面的加速路径；
	// 打洞失败或隧道断开时自动回退到 TCP 中继，二层不断连。
	if c.cfg.P2P && c.tunnel == nil {
		c.startP2PConnect()
	}

	c.logger.Printf("vrndis client: 已连接 %s，本机地址 %s/%d 网关 %s",
		c.cfg.Server, assign.IP, assign.Prefix, assign.Gateway)
	return &assign, nil
}

// Assignment 当前分配结果（未连接时为 nil）。
func (c *Client) Assignment() *proto.Assignment {
	c.assignMu.RLock()
	defer c.assignMu.RUnlock()
	return c.assign
}

// peerInfo 一次收到的对端候选信息。同一对端可携带多个候选地址
// （srflx / UPnP 映射 / LAN host / IPv6），按 MAC 聚合后并发打洞。
type peerInfo struct {
	MAC   net.HardwareAddr
	Addrs []*net.UDPAddr
}

// waitForPeerCandidate 阻塞等待服务器下发 TypePeerCandidate 消息，并在
// 收到首个候选后的短窗口内继续收集（服务器会下发/推送多个 peer 的候选，
// 客户端上线时序错开时，对端真实候选可能稍晚到达）。
// 由 sockToDev 在收到消息后通过 peerCandCh 投递，避免两个 goroutine 竞争读
// 同一个连接导致消息丢失。
func (c *Client) waitForPeerCandidate(ctx context.Context) ([]*peerInfo, error) {
	c.connMu.Lock()
	conn := c.conn
	c.connMu.Unlock()
	if conn == nil {
		return nil, errors.New("vrndis/client: 无活跃连接，无法等待 peer 候选")
	}

	var out []*peerInfo
	for {
		var timer <-chan time.Time
		if len(out) > 0 {
			// 已有候选：再给 300ms 收集后续候选（同 MAC 去重）
			t := time.NewTimer(300 * time.Millisecond)
			defer t.Stop()
			timer = t.C
		}
		select {
		case <-ctx.Done():
			if len(out) > 0 {
				return out, nil
			}
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, errors.New("超时：时限内无对端应答")
			}
			return nil, ctx.Err()
		case <-c.stopCh:
			return nil, errors.New("vrndis/client: 客户端已停止")
		case pi := <-c.peerCandCh:
			if len(pi.Addrs) == 0 {
				continue
			}
			merged := false
			for _, e := range out {
				if bytes.Equal(e.MAC, pi.MAC) {
					// 同一 peer 的多条候选（srflx/LAN/v6）合并到一起，
					// 供打洞阶段并发尝试。丢弃会损失内网直连候选。
					e.Addrs = append(e.Addrs, pi.Addrs...)
					merged = true
					break
				}
			}
			if !merged {
				out = append(out, pi)
			}
		case <-timer:
			return out, nil
		}
	}
}

// setP2PActive 标记 p2pConnect 是否在运行（事件驱动重启的去抖依据）。
func (c *Client) setP2PActive(v bool) {
	c.p2pMu.Lock()
	c.p2pActive = v
	c.p2pMu.Unlock()
}

// isP2PActive 返回 p2pConnect 是否在运行。
func (c *Client) isP2PActive() bool {
	c.p2pMu.Lock()
	defer c.p2pMu.Unlock()
	return c.p2pActive
}

// hasTunnel 返回当前是否存在活跃 P2P 隧道。
func (c *Client) hasTunnel() bool {
	c.p2pMu.Lock()
	tun := c.tunnel
	c.p2pMu.Unlock()
	return tun != nil
}

// startP2PConnect 抢占式启动一轮打洞：通过 p2pActive/隧道/退避三重检查后
// 同步置位 p2pActive（防止广播多条候选推送各起一个 goroutine 的风暴），
// goroutine 退出时复位。连接路径与事件驱动重试均经此入口。
func (c *Client) startP2PConnect() {
	c.p2pMu.Lock()
	if c.p2pActive || c.tunnel != nil || time.Now().Before(c.p2pBackoff) {
		c.p2pMu.Unlock()
		return
	}
	c.p2pActive = true
	c.p2pMu.Unlock()
	c.wg.Add(1)
	go func() {
		defer c.setP2PActive(false)
		c.p2pConnect()
	}()
}

// failP2PRound 记录一次打洞失败：失败计数 +1 并按 1m/2m/4m/5m 退避，
// 退避到期后自动发起下一轮——不依赖对端事件，保证间歇可达的网络环境
// （如对端周期性上线）最终能自我恢复。
//
// 日志策略（避免每轮刷屏）：首轮失败输出一次性诊断结论（常见原因），
// 之后每轮仅输出一行退避摘要（轮数 + 下次尝试间隔 + 当前走中继）。
func (c *Client) failP2PRound(reason string) {
	c.p2pMu.Lock()
	c.p2pFailures++
	n := c.p2pFailures
	delay := time.Minute
	switch {
	case n == 2:
		delay = 2 * time.Minute
	case n == 3:
		delay = 4 * time.Minute
	case n >= 4:
		delay = 5 * time.Minute
	}
	c.p2pBackoff = time.Now().Add(delay)
	c.p2pMu.Unlock()
	if n == 1 {
		c.logger.Printf("vrndis client: P2P 首轮失败: %s（已回退 TCP 中继，二层网络不受影响）", reason)
		c.logger.Printf("vrndis client: 常见原因: 对端未上线 / 对称 NAT / 本 NAT 无 hairpin（同 NAT 设备互打需 hairpin）")
	} else {
		c.logger.Printf("vrndis client: P2P 连续 %d 轮失败（最近: %s），继续走中继，%v 后重试", n, reason, delay.Round(time.Second))
	}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		select {
		case <-c.stopCh:
		case <-time.After(delay):
			c.startP2PConnect()
		}
	}()
}

// resetP2PBackoff 打洞成功后清零连续失败计数与退避时间。
func (c *Client) resetP2PBackoff() {
	c.p2pMu.Lock()
	c.p2pFailures = 0
	c.p2pBackoff = time.Time{}
	c.p2pMu.Unlock()
}

// withdrawCandidate 通知服务器撤销本机候选。打洞失败/等待超时/回退中继
// 时调用，避免死候选长期残留在服务器 peer 表（后来节点会向已关闭的端点
// 空打洞 30 秒）。
func (c *Client) withdrawCandidate() {
	conn := c.currentConn()
	if conn == nil {
		return
	}
	c.assignMu.RLock()
	mac := c.assign.MAC
	c.assignMu.RUnlock()
	if mac == "" {
		return
	}
	body, _ := json.Marshal(proto.P2PWithdraw{
		Version: proto.ProtocolVersion,
		MAC:     mac,
	})
	_ = c.send(conn, proto.TypeP2PWithdraw, body)
}

// drainPeerCands 清空 peerCandCh 中积压的陈旧推送。
func (c *Client) drainPeerCands() {
	for {
		select {
		case <-c.peerCandCh:
			continue
		default:
			return
		}
	}
}

// candidateAddr 返回 P2P 打洞的目标地址。
// 优先使用显式配置的 --stun-server；否则默认走服务器的公网地址 + 5478：
// STUN 请求必须经物理网络直接送达，服务器（NAT 外侧）才能看到客户端
// 真实映射；若经隧道中继，看到的源地址只是隧道内网地址，候选无效。
func (c *Client) candidateAddr() string {
	if c.cfg.STUNServer != "" {
		return c.cfg.STUNServer
	}
	if host, _, err := net.SplitHostPort(c.cfg.Server); err == nil {
		return net.JoinHostPort(host, "5478")
	}
	return ""
}

// currentConn 获取当前活跃连接（并发安全）。
func (c *Client) currentConn() net.Conn {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	return c.conn
}

// setTunnel 设置当前 P2P 隧道（打洞成功时调用）。
func (c *Client) setTunnel(t *p2p.Tunnel, peerMAC net.HardwareAddr) {
	c.p2pMu.Lock()
	defer c.p2pMu.Unlock()
	c.tunnel = t
	c.peerMAC = peerMAC
	c.peering = false
}

// getPeerMAC 当前对端 MAC（并发安全，测试用）。
func (c *Client) getPeerMAC() net.HardwareAddr {
	c.p2pMu.Lock()
	defer c.p2pMu.Unlock()
	return c.peerMAC
}

// getTunnel 当前 P2P 隧道（并发安全，测试用）。
func (c *Client) getTunnel() *p2p.Tunnel {
	c.p2pMu.Lock()
	defer c.p2pMu.Unlock()
	return c.tunnel
}

// clearTunnel 清除当前隧道（回退/断线时调用），返回之前持有的隧道。
func (c *Client) clearTunnel() *p2p.Tunnel {
	c.p2pMu.Lock()
	defer c.p2pMu.Unlock()
	t := c.tunnel
	c.tunnel = nil
	c.peerMAC = nil
	c.peering = false
	return t
}

// isPeering 是否正在打洞中（避免重复打洞）。
func (c *Client) isPeering() bool {
	c.p2pMu.Lock()
	defer c.p2pMu.Unlock()
	return c.peering
}

// p2pConnect P2P 打洞流程：Discover -> 上报候选 -> 等待对端候选 -> 打洞 -> 建隧道。
// 失败/超时自动回退 TCP 中继（无需任何动作，devToSock 始终写入 TCP）。
func (c *Client) p2pConnect() {
	defer c.wg.Done()

	stun := c.candidateAddr()
	if stun == "" {
		c.logger.Printf("vrndis client: P2P 未配置 STUN 服务器，回退 TCP 中继")
		return
	}
	// 清理上一轮残留：撤销仍旧登记在服务器上的旧候选，并丢弃等待通道里
	// 积压的陈旧推送，防止新一轮拿到过期端点。
	c.withdrawCandidate()
	c.drainPeerCands()

	// 1) 通过 STUN 获取本机 NAT 映射后的公网地址。
	// ctx 联动 stopCh：Stop 时立即取消 STUN/候选等待/打洞，避免
	// 客户端关闭后 punch 窗口空转 30 秒才退出。
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cancelOnStop := make(chan struct{})
	defer close(cancelOnStop)
	go func() {
		select {
		case <-c.stopCh:
			cancel()
		case <-cancelOnStop:
		}
	}()
	stunConn, localAddr, err := p2p.Discover(ctx, stun, 0)
	if err != nil {
		c.logger.Printf("vrndis client: STUN Discover 失败: %v（回退 TCP 中继）", err)
		return
	}
	srflx := net.JoinHostPort(localAddr.IP.String(), strconv.Itoa(localAddr.Port))
	c.logger.Printf("vrndis client: 本地候选 %v", srflx)

	// 2) 收集本机全部候选并上报。
	// 候选共用 STUN Discover 的同一 socket（srflx 端口即打洞端口）：
	//   - Candidate（srflx 公网映射）：兼容旧服务器的单候选协议
	//   - host 候选：本机 LAN 私网 v4 + 公网 v6，同 NAT 设备可内网直连，
	//     绕开本 NAT 无 hairpin 的物理死路。
	// 候选端口必须严格等于 socket 本地端口（PunchConn 复用此 socket），
	// 否则候选指向无人监听的端点。
	advList := []string{srflx}
	c.assignMu.RLock()
	selfMAC := c.assign.MAC
	selfIP := net.ParseIP(c.assign.IP)
	c.assignMu.RUnlock()
	if selfMAC == "" {
		return
	}
	// 本机隧道网段（与本机隧道 IP 一并排除，防 TAP 网段地址混入候选）；
	// Prefix<=0 时跳过（/0 会全量排除所有地址）
	var tunnelNet *net.IPNet
	if c.assign.Prefix > 0 {
		if _, n, err := net.ParseCIDR(selfIP.String() + "/" + strconv.Itoa(c.assign.Prefix)); err == nil {
			tunnelNet = n
		}
	}
	if uaddr, ok := stunConn.LocalAddr().(*net.UDPAddr); ok {
		if hosts, herr := p2p.HostCandidates(uaddr.Port, selfIP, tunnelNet); herr == nil {
			advList = append(advList, hosts...)
		} else {
			c.logger.Printf("vrndis client: host 候选收集失败: %v", herr)
		}
	}
	// 2.5) 网关端口映射（UPnP IGD / NAT-PMP）：为对称 NAT 争取一个固定
	// 外部端口的显式 full-cone 映射。映射 host 复用 srflx 公网 IP（网关
	// WAN 地址；多出口环境小概率失配，作为多候选之一失败无害）。
	// 失败静默：无 UPnP 的网络照常纯 STUN 打洞，不产生额外候选。
	if uaddr, ok := stunConn.LocalAddr().(*net.UDPAddr); ok {
		if extPort := p2p.MapPort(uaddr.Port, c.logger); extPort != 0 {
			advList = append(advList, net.JoinHostPort(localAddr.IP.String(), strconv.Itoa(extPort)))
		}
	}
	if extra := len(advList) - 1; extra > 0 {
		c.logger.Printf("vrndis client: 候选多路化: srflx=%s, %d 个额外候选 %v", srflx, extra, advList[1:])
	}

	conn := c.currentConn()
	if conn != nil {
		body, _ := json.Marshal(proto.P2PAdvertise{
			Version:    proto.ProtocolVersion,
			MAC:        selfMAC,
			Candidate:  srflx,
			Candidates: advList,
		})
		_ = c.send(conn, proto.TypeP2PAdvertise, body)
	}

	// 2.5) 主动拉取当前在线候选：服务器只在新注册时推送，错过的历史候选
	// 要靠 query 拿回（answerPeerQuery 应答全量候选）。
	if conn := c.currentConn(); conn != nil {
		qbody, _ := json.Marshal(proto.PeerQuery{Version: proto.ProtocolVersion})
		_ = c.send(conn, proto.TypePeerQuery, qbody)
	}

	// 3) 等待对端候选（带超时），超时则回退
	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel2()
	go func() {
		select {
		case <-c.stopCh:
			cancel2()
		case <-cancelOnStop:
		}
	}()
	peers, err := c.waitForPeerCandidate(ctx2)
	if err != nil {
		c.withdrawCandidate()
		c.failP2PRound(fmt.Sprintf("等待对端候选失败: %v", err))
		return
	}
	for _, p := range peers {
		c.logger.Printf("vrndis client: 收到对端 %s 候选 %v", p.MAC, p.Addrs)
	}

	// 4) 打洞
	c.p2pMu.Lock()
	c.peering = true
	c.p2pMu.Unlock()
	var cands []*net.UDPAddr
	seen := map[string]bool{}
	for _, p := range peers {
		// answerPeerQuery 不排除调用方自己的候选，过滤本机端点
		if p.MAC.String() == selfMAC {
			continue
		}
		for _, a := range p.Addrs {
			if a == nil || a.Port == 0 || seen[a.String()] {
				continue // 端口 0 是空候选遗存（ResolveUDPAddr("") 不报错）
			}
			seen[a.String()] = true
			cands = append(cands, a)
		}
	}
	if len(cands) == 0 {
		c.withdrawCandidate()
		c.failP2PRound("无对端候选（仅本机）")
		return
	}
	tun, err := p2p.PunchConn(ctx2, stunConn, cands)
	if err != nil {
		c.withdrawCandidate()
		c.clearTunnel()
		c.failP2PRound(fmt.Sprintf("打洞失败: %v", err))
		return
	}
	// 从实际打通的来源回查对端 MAC（候选回退匹配：NAT 后 from 即候选映射地址）
	var peerMAC net.HardwareAddr
	if tun.Peer() != nil {
		for _, p := range peers {
			for _, a := range p.Addrs {
				if a != nil && a.String() == tun.Peer().String() {
					peerMAC = p.MAC
					break
				}
			}
			if peerMAC != nil {
				break
			}
		}
	}
	if peerMAC == nil && len(peers) > 0 {
		peerMAC = peers[0].MAC
	}
	c.setTunnel(tun, peerMAC)
	c.resetP2PBackoff()
	// 通知服务器打洞成功（仅管理面展示用，失败无碍）
	if conn := c.currentConn(); conn != nil && peerMAC != nil {
		body, _ := json.Marshal(proto.P2PReady{
			Version: proto.ProtocolVersion,
			PeerMAC: peerMAC.String(),
		})
		_ = c.send(conn, proto.TypeP2PReady, body)
	}
	c.logger.Printf("vrndis client: P2P 隧道已建立 -> %v", tun.Peer())

	// 5) 隧道读循环：UDP 隧道 -> 本地网卡；隧道断开（空闲超时或对端关闭）
	// 时退出，自动回退 TCP 中继（二层不断连）。AttachIdleMonitor 负责空闲检测。
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		for {
			frame, err := tun.Read()
			if err != nil {
				return
			}
			if err := c.dev.Write(frame); err != nil {
				if errors.Is(err, tap.ErrBufferFull) {
					continue
				}
				c.logger.Printf("vrndis client: P2P 隧道写入本地网卡失败: %v", err)
				return
			}
		}
	}()

	cancelIdle := tun.AttachIdleMonitor(5*time.Second, c.logger)
	<-tun.Done()
	cancelIdle()

	c.clearTunnel()
	if tun != nil {
		_ = tun.Close()
	}
	c.logger.Printf("vrndis client: P2P 隧道断开，已回退 TCP 中继")
}

// Run 连接并运行直到 ctx 取消或整体 Stop。
//
// 若 cfg.Reconnect 开启，断线后按指数退避自动重连（重连成功重新 Hello/Assign
// 并刷新网卡）；重连次数达到 MaxRetries（>0）仍失败时返回最后一次错误。
func (c *Client) Run(ctx context.Context) error {
	if !c.cfg.Reconnect {
		// 单连接模式（与旧行为一致）。
		if _, err := c.connectOnce(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			c.Stop()
			return ctx.Err()
		case <-c.stopCh:
			return nil
		case <-c.connDone:
			c.Stop()
			return errors.New("vrndis/client: 连接关闭")
		}
	}

	backoff := c.cfg.ReconnectBackoff
	if backoff <= 0 {
		backoff = defaultReconnectBackoff
	}
	maxBackoff := c.cfg.ReconnectMaxBackoff
	if maxBackoff <= 0 {
		maxBackoff = defaultMaxReconnectBoff
	}
	failures := 0
	for {
		if _, err := c.connectOnce(); err != nil {
			failures++
			c.logger.Printf("vrndis client: 连接失败（第 %d 次）: %v", failures, err)
			if c.cfg.MaxRetries > 0 && failures >= c.cfg.MaxRetries {
				c.Stop()
				return fmt.Errorf("vrndis/client: 重连 %d 次仍失败: %w", failures, err)
			}
		} else {
			failures = 0
			c.logger.Printf("vrndis client: 已重连 %s，退避已重置", c.cfg.Server)
			select {
			case <-ctx.Done():
				c.Stop()
				return ctx.Err()
			case <-c.stopCh:
				return nil
			case <-c.connDone:
				c.logger.Printf("vrndis client: 连接断开，准备重连")
			}
		}

		select {
		case <-ctx.Done():
			c.Stop()
			return ctx.Err()
		case <-c.stopCh:
			return nil
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// Stop 优雅下线并回收资源。可重复调用、可从任意错误路径调用。
// 关闭整体生命周期：发送 BYE、关闭当前连接、关闭共享网卡、等待所有协程退出。
func (c *Client) Stop() {
	c.closeOnce.Do(func() { close(c.stopCh) })

	c.connMu.Lock()
	conn := c.conn
	c.connMu.Unlock()
	// 显式关闭隧道：keepalive 会让 tunnel 永不空闲（idle monitor 不会触发），
	// 不 Close 的话 p2pConnect 会一直卡在 <-tun.Done()，Stop 无法返回。
	if t := c.clearTunnel(); t != nil {
		_ = t.Close()
	}
	if conn != nil {
		// 尽力而为发 BYE（带写超时，避免慢对端卡住退出）。
		_ = conn.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
		c.writeMu.Lock()
		_ = proto.WriteMessage(conn, proto.TypeBye, nil)
		c.writeMu.Unlock()
		c.teardownConn(conn)
	}
	c.connWG.Wait()
	if c.dev != nil {
		_ = c.dev.Close()
	}
	c.wg.Wait()
}

// teardownConn 结束单个连接的生命周期：关闭其结束信号与 socket。
// 不关闭共享网卡（重连复用）。可重复调用（幂等）。
func (c *Client) teardownConn(conn net.Conn) {
	c.connMu.Lock()
	if c.conn == conn {
		c.connOnce.Do(func() { close(c.connDone) })
		conn.Close()
		c.conn = nil // 断开期数据面不再写入旧连接，直接丢帧
	}
	c.connMu.Unlock()
}

// teardownCurrentConn 断开当前连接(若存在)，用于设备侧不可恢复错误:
// 关闭 connDone 使 Run() 走正常重连路径，避免单向僵尸隧道。
func (c *Client) teardownCurrentConn() {
	c.connMu.Lock()
	conn := c.conn
	c.connMu.Unlock()
	if conn != nil {
		c.teardownConn(conn)
	}
}

// Closed 连接是否已终止（断线检测）：整体已 Stop 或当前连接已断开。
func (c *Client) Closed() bool {
	select {
	case <-c.stopCh:
		return true
	default:
	}
	c.connMu.Lock()
	done := c.connDone
	c.connMu.Unlock()
	if done == nil {
		return true // 从未成功建立连接
	}
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// ---- 内部 ----

func (c *Client) send(conn net.Conn, typ byte, body []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.crypto != nil {
		plaintext := append([]byte{typ}, body...)
		frame, err := c.crypto.Encrypt(plaintext)
		if err != nil {
			return err
		}
		return proto.WriteMessage(conn, proto.TypeData, frame)
	}
	return proto.WriteMessage(conn, typ, body)
}

// notePong 记录最近一次对端活跃时间（收到 Pong/Ping 均视为活跃）。
func (c *Client) notePong() {
	c.lastPongMu.Lock()
	c.lastPong = time.Now()
	c.lastPongMu.Unlock()
}

// sockToDev 该连接的读方向：socket → 本地网卡。连接断开/收到 BYE 时结束本连接。
func (c *Client) sockToDev(conn net.Conn) {
	defer c.connWG.Done()
	for {
		msg, err := proto.ReadMessage(conn)
		if err != nil {
			select {
			case <-c.stopCh:
			case <-c.connDone:
			default:
				c.logger.Printf("vrndis client: 连接关闭: %v", err)
			}
			c.teardownConn(conn)
			return
		}
		// 加密信封：启用 PSK 时所有消息以 TypeData 外层类型传输，
		// 密文 = 加密([真实类型]+[真实body])。先解包还原真实类型，再 dispatch；
		// 否则控制消息（Ping/BYE 等空 body 消息）会被误当数据帧写网卡。
		if c.crypto != nil && msg.Type == proto.TypeData {
			plaintext, err := c.crypto.Decrypt(msg.Body)
			if err != nil {
				c.logger.Printf("vrndis client: 解密失败: %v", err)
				c.teardownConn(conn)
				return
			}
			if len(plaintext) == 0 {
				c.teardownConn(conn)
				return
			}
			msg.Type = plaintext[0]
			msg.Body = plaintext[1:]
		}
		switch msg.Type {
		case proto.TypeData:
			if err := c.dev.Write(msg.Body); err != nil {
				if errors.Is(err, tap.ErrBufferFull) {
					// 背压：网卡队列已满，丢帧继续，不断连（与服务器 tapWriter 一致）。
					c.logger.Printf("vrndis client: 网卡队列满，丢帧: %v", err)
					continue
				}
				c.logger.Printf("vrndis client: 写入本地网卡失败: %v", err)
				c.teardownConn(conn)
				return
			}
		case proto.TypeBye:
			c.teardownConn(conn)
			return
		case proto.TypePing:
			// 服务器 ping 我们 → 回 Pong；同时视为活跃。
			c.notePong()
			c.send(conn, proto.TypePong, nil)
		case proto.TypePong:
			// 心跳应答。
			c.notePong()
		case proto.TypeManagement:
			// 处理管理消息，当前忽略。
		case proto.TypePeerCandidate:
			// 把对端候选投递给 p2pConnect 的 waitForPeerCandidate。
			var pc proto.PeerCandidate
			if json.Unmarshal(msg.Body, &pc) == nil {
				// 多候选优先；旧服务器只填单 Candidate 时回退
				list := pc.Candidates
				if len(list) == 0 {
					if pc.Candidate == "" {
						// 在线但未上报候选（relay-only 诊断条目，含全空结束标记）：
						// 不参与打洞，直接丢弃，避免向 0.0.0.0:0 空打洞。
						break
					}
					list = []string{pc.Candidate}
				}
				mac, err := ethernet.MACFromString(pc.PeerMAC)
				if err != nil {
					break
				}
				var addrs []*net.UDPAddr
				for _, cs := range list {
					// 坑：ResolveUDPAddr("udp","") 返回 :0 且 err==nil，
					// 空候选会变 0.0.0.0:0 废候选，必须按端口 0 过滤。
					a, err2 := net.ResolveUDPAddr("udp", cs)
					if err2 == nil && a != nil && a.Port != 0 {
						addrs = append(addrs, a)
					}
				}
				if len(addrs) > 0 {
					select {
					case c.peerCandCh <- &peerInfo{MAC: mac, Addrs: addrs}:
					default:
						c.logger.Printf("vrndis client: peer 候选通道满，丢弃")
					}
				}
			}
		}

		// 事件驱动重试：p2pConnect 未运行且当前无隧道（如上一轮打洞
		// 已超时回退）时，新候选到来即重启打洞流程，覆盖"本端先就绪、
		// 对端后上线"的时序错开场景。
		if c.cfg.P2P {
			select {
			case <-c.stopCh:
			default:
				// 退避期内的事件直接丢弃；退避到期后由 failP2PRound
				// 安排的定时器自动发起下一轮，不依赖对端事件。
				c.startP2PConnect()
			}
		}
	}
}

// shouldRouteViaP2P 判断一帧是否应走 P2P 隧道。
// 仅当隧道已激活且帧的目的 MAC 是对端虚拟 MAC 时走 UDP 直连；
// 其余（网关/广播/上网帧）走 TCP 中继，保证控制面与回退路径始终可用。
func (c *Client) shouldRouteViaP2P(frame []byte) bool {
	c.p2pMu.Lock()
	tun := c.tunnel
	peer := c.peerMAC
	c.p2pMu.Unlock()
	if tun == nil || peer == nil {
		return false
	}
	f, err := ethernet.ParseFrame(frame)
	if err != nil {
		return false
	}
	return bytes.Equal(f.Dst, peer)
}

// tryWriteP2P 尝试通过 P2P 隧道写出一帧。
// 返回 true 表示已写出（无论成功/失败，不再走 TCP 中继以避免重复投递）；
// 返回 false 表示无隧道，应走 TCP 中继。
func (c *Client) tryWriteP2P(frame []byte) bool {
	c.p2pMu.Lock()
	tun := c.tunnel
	c.p2pMu.Unlock()
	if tun == nil {
		return false
	}
	if err := tun.Write(frame); err != nil {
		// 隧道写入失败：触发回退（清除隧道），后续帧自动走 TCP 中继。
		// 不回写 TCP 以避免切换瞬间的重复投递与乱序。
		c.logger.Printf("vrndis client: P2P 隧道写入失败: %v，回退 TCP 中继", err)
		c.clearTunnel()
		if tun != nil {
			_ = tun.Close()
		}
	}
	return true
}

// devToSock 数据面写方向：本地网卡 → socket。长期驻留，跨重连复用。
// 未连接（重连等待期）时收到的帧直接丢弃。
//
// P2P 分流：目的 MAC 为对端时走 UDP 隧道；其余走 TCP 中继。
func (c *Client) devToSock() {
	defer c.wg.Done()
	for {
		frame, err := c.dev.Read()
		if err != nil {
			select {
			case <-c.stopCh:
				return // 正在停止，安静退出
			default:
			}
			// 设备错误(含 wintun 0xC0000005，已由 tap 层归一为 io.EOF)
			// -> 断开当前连接，走 Run() 正常重连路径；不 panic、不留僵尸隧道
			c.logger.Printf("vrndis client: 读取本地网卡失败，断开重连: %v", err)
			c.teardownCurrentConn()
			return
		}
		// P2P 分流：目的 MAC 为对端时走 UDP 隧道，其余走 TCP 中继。
		// 中继路径始终存在（TCP 连接常驻），故分流失败只丢帧、不断连。
		if c.shouldRouteViaP2P(frame) && c.tryWriteP2P(frame) {
			continue
		}
		c.connMu.Lock()
		conn := c.conn
		c.connMu.Unlock()
		if conn == nil {
			continue // 未连接，丢帧
		}
		if c.crypto != nil {
			plaintext := append([]byte{proto.TypeData}, frame...)
			var err error
			frame, err = c.crypto.Encrypt(plaintext)
			if err != nil {
				c.logger.Printf("vrndis client: 加密失败: %v", err)
				continue
			}
		}
		c.writeMu.Lock()
		err = proto.WriteMessage(conn, proto.TypeData, frame)
		c.writeMu.Unlock()
		if err != nil {
			c.connMu.Lock()
			isCurrent := c.conn == conn
			c.connMu.Unlock()
			if isCurrent {
				c.logger.Printf("vrndis client: 发送失败，连接已断开: %v", err)
				c.teardownConn(conn)
			}
		}
	}
}

// heartbeat 主动断线检测：周期性 Ping，超时未收到 Pong/Ping 判定断线并结束本连接。
func (c *Client) heartbeat(conn net.Conn) {
	defer c.connWG.Done()
	interval := c.cfg.HeartbeatInterval
	if interval <= 0 {
		interval = defaultHeartbeatInterval
	}
	timeout := c.cfg.HeartbeatTimeout
	if timeout <= 0 {
		timeout = defaultHeartbeatTimeout
	}
	c.notePong() // 建立基线
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-c.connDone:
			return
		case <-c.stopCh:
			return
		case <-t.C:
			c.lastPongMu.Lock()
			stale := time.Since(c.lastPong) > timeout
			c.lastPongMu.Unlock()
			if stale {
				c.logger.Printf("vrndis client: 心跳超时（%s 无响应），判定断线", timeout)
				c.teardownConn(conn)
				return
			}
			// 发送 Ping 探测。
			c.writeMu.Lock()
			_ = proto.WriteMessage(conn, proto.TypePing, nil)
			c.writeMu.Unlock()
		}
	}
}
