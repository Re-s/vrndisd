package p2p

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"vrndis/internal/ethernet"
)

var errNoXORAddr = errors.New("vrndis/p2p: bad XOR-MAPPED-ADDRESS")

// maxUDPFrame 单个 UDP 数据报承载帧的最大字节数（覆盖标准以太网帧与常见 jumbo）。
const maxUDPFrame = 9000

// probeMagic 打洞/保活探测帧。隧道建立后 keepalive 会周期向对端发送该探测包
// 以刷新 NAT 映射。探测帧不是合法以太网帧，Tunnel.Read 会在数据面吸收（跳过）它，
// 避免把探测帧当作 L2 数据写入 TAP。
var probeMagic = []byte("vrndis-p2p-probe\x00\x01")

// keepaliveInterval 保活探测发送间隔。
// 维持 NAT 映射新鲜只需秒级频率（NAT UDP 映射空闲超时通常数十秒）；
// 过去用 500ms 只会增加无效空转流量。
const keepaliveInterval = 2 * time.Second

// probeReplyMinInterval 是对同一来源回复探测帧的最小间隔。
// 隧道建立后 Read 收到对端探测会原路回一个探测帧（供 ping 诊断测直连
// RTT、并让对端确认本侧存活）。但"收 probe 即回 probe"会让双方互答
// 回包形成无速率限制的乒乓自激：每收到一个包就触发一个回包，双向
// 背靠背狂发数万 pps（实测 6µs 包间隔、~27k pps）。按来源地址限速即
// 可断开自激回路——后到的重复探测不再触发回复。
const probeReplyMinInterval = time.Second

// probeReplyMaxSources 记录的来源地址上限，避免被扫描来源撑爆内存。
const probeReplyMaxSources = 64

// maxPeers 已验证对端地址上限（同上，防扫描来源撑爆保活目标集）。
const maxPeers = 64

// punchCollectWindow 首个对端回包后继续收集其余对端的时间窗口。
// 过短会漏掉回包较慢（CGNAT 丢包重传、跨洲链路）的对端，使"能直连
// 的 peer 只有第一个连上隧道"退化回单对端行为；过长则延迟首条隧道
// 可用。2.5s 覆盖一轮 100ms 探测重发 + 一次丢包重传。
const punchCollectWindow = 2500 * time.Millisecond

// ErrNoRoute 帧的目的 MAC 没有已注册的直连路由。
// 调用方应改走 TCP 中继；这只是一个对端未直连，隧道本身与其他对端不受影响。
var ErrNoRoute = errors.New("vrndis/p2p: no direct route for destination MAC")

// isProbe 判断一个数据报是否为打洞/保活探测帧。
func isProbe(b []byte) bool {
	return len(b) == len(probeMagic) && string(b) == string(probeMagic)
}

// Tunnel 一个已打通的 UDP 隧道，用同一个 socket 服务多个对端。
//
// UDP 无连接：一个 socket 可同时与任意多对端通信。打洞窗口内收到的
// 所有来源都进入 addrs（已验证可达——能给我们发包的地址必然能回包），
// 调用方按候选把对端 MAC 注册进 routes 后，数据帧按目的 MAC 直送
// 对应对端。由此摆脱"单隧道单对端"限制：ping 任何已打通的 peer 都
// 走直连，而不是只有一个先回包的幸运儿独占隧道。
//
// 主对端取打洞窗口内首个回包来源（peerAddr，兼容旧 API 与管理面诊断）。
// 提供与 tap.Device 类似的 Read/Write 语义，可替换服务器中继数据面。
type Tunnel struct {
	conn *net.UDPConn

	done      chan struct{}
	closeOnce sync.Once

	writeMu sync.Mutex

	mu       sync.Mutex
	peerAddr *net.UDPAddr            // 首个打通的对端（兼容旧 API/管理面诊断）
	routes   map[string]*net.UDPAddr // MAC 字符串 -> 对端地址（数据帧路由）
	addrs    map[string]*net.UDPAddr // 已验证可达对端地址集（keepalive 目标，收包即学习）

	// lastRx 记录最近一次收到数据的时间（受 lastRxMu 保护），
	// 供 AttachIdleMonitor 轮询判断隧道是否空闲；不要再读 socket。
	lastRx   time.Time
	lastRxMu sync.Mutex

	// lastTx 记录最近一次发出数据的时间（同锁保护，供本机管理面诊断）。
	lastTx   time.Time
	lastTxMu sync.Mutex

	// probeReply 按来源地址记录最近一次回复探测帧的时间，
	// 用于抑制"收到探测即回复"造成的乒乓自激与反射放大。
	probeReply   map[string]time.Time
	probeReplyMu sync.Mutex
}

// Punch 向 peerCandidates 打洞：持续向候选地址发送探测包，并等待收到对端探测。
// 一旦收到来自任一候选来源的数据报，即视为双向打通（对端已向我们发包、我们已向其发包），
// 返回已建立的 Tunnel。若在 ctx 截止前未打通，返回 ErrPunchTimeout。
func Punch(ctx context.Context, localPort int, peerCandidates []*net.UDPAddr) (*Tunnel, error) {
	local := &net.UDPAddr{IP: net.IPv4zero, Port: localPort}
	conn, err := net.ListenUDP("udp4", local)
	if err != nil {
		return nil, fmt.Errorf("vrndis/p2p: listen punch: %w", err)
	}
	return punchConn(ctx, conn, peerCandidates)
}

// PunchConn 在已绑定的 UDP 连接上打洞。conn 交由 Tunnel 持有，打通后由调用方关闭。
// 供测试在并发场景下显式绑定端口、避免端口复用竞态。
//
// 打洞窗口：首个对端回包后并不立即返回，而是继续收集 punchCollectWindow
// 时间内的其余回包来源，让一条隧道同时携带多个对端。
func PunchConn(ctx context.Context, conn *net.UDPConn, peerCandidates []*net.UDPAddr) (*Tunnel, error) {
	return punchConn(ctx, conn, peerCandidates)
}

func punchConn(ctx context.Context, conn *net.UDPConn, peerCandidates []*net.UDPAddr) (*Tunnel, error) {
	return punchConnWindow(ctx, conn, peerCandidates, punchCollectWindow)
}

// punchConnWindow 是 punchConn 的可参数化实现（collectWindow 供测试调小，
// 避免每个用例空等 2.5 秒）。
func punchConnWindow(ctx context.Context, conn *net.UDPConn, peerCandidates []*net.UDPAddr, collectWindow time.Duration) (*Tunnel, error) {
	// lastRx 供 AttachIdleMonitor 判断空闲（真正的刷新由 Tunnel.Read 的 touchRx 完成）。
	t := &Tunnel{
		conn:       conn,
		done:       make(chan struct{}),
		lastRx:     time.Now(),
		routes:     make(map[string]*net.UDPAddr),
		addrs:      make(map[string]*net.UDPAddr, 8),
		probeReply: make(map[string]time.Time, 8),
	}

	dbg := log.New(os.Stderr, "punchdbg(local="+conn.LocalAddr().String()+"): ", log.LstdFlags|log.Lmicroseconds)
	dbg.Printf("start, candidates=%v", peerCandidates)

	sentOnce := false
	// 立即发一轮探测：避免等待首个 ticker（100ms）期间收到对端探测
	// 却因 sentOnce=false 被丢弃，造成双方互丢的活锁。
	sendProbe := func() {
		for _, cand := range peerCandidates {
			if cand == nil {
				continue
			}
			if _, werr := conn.WriteToUDP(probeMagic, cand); werr == nil {
				sentOnce = true
			}
		}
	}
	sendProbe()

	// 首个对端回包后启动收集窗口：窗口到期且已有主对端即返回 Tunnel。
	collectFired := make(chan struct{}, 1)
	var collectStarted bool
	startCollect := func() {
		if collectStarted {
			return
		}
		collectStarted = true
		time.AfterFunc(collectWindow, func() {
			select {
			case collectFired <- struct{}{}:
			default:
			}
		})
	}

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			conn.Close()
			return nil, ErrPunchTimeout
		case <-ticker.C:
			sendProbe()
		case <-collectFired:
			// 收集窗口结束：以首个对端为主对端建立隧道。窗口到期时
			// peerAddr 必然非空（startCollect 仅在 setPeer 后调用）。
			_ = conn.SetReadDeadline(time.Time{})
			dbg.Printf("OK primary=%v, collected %d peers total", t.peerAddr, len(t.addrs))
			t.startKeepalive()
			return t, nil
		default:
		}
		// 轮询读取：一帧即确认打通。
		conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		buf := make([]byte, maxUDPFrame)
		n, from, rerr := conn.ReadFromUDP(buf)
		if rerr != nil {
			dbg.Printf("read err: %v", rerr)
			continue
		}
		dbg.Printf("got %d bytes from %v", n, from)
		if n == 0 {
			continue
		}
		// 只有确保已向对端发出过探测，才视为双向打通。
		// 否则"先收到对端探测"的一侧可能在另一侧收到本侧探测前就停发，导致对端超时。
		if !sentOnce {
			dbg.Printf("dropping packet: sentOnce=false")
			continue
		}
		if t.peerAddr == nil {
			t.setPeer(from)
			dbg.Printf("primary peer=%v", from)
			startCollect()
		}
		// 任何来源都记为可达：它给我们发了包，我们必能回包给它。
		t.addPeer(from)
	}
}

func (t *Tunnel) setPeer(from *net.UDPAddr) {
	t.mu.Lock()
	t.peerAddr = from
	t.mu.Unlock()
}

// addPeer 记录一个已验证可达的对端地址（收包来源必然可达）。
// 超过上限时整体重置（防扫描来源撑爆保活目标集），返回是否新增。
func (t *Tunnel) addPeer(from *net.UDPAddr) bool {
	if from == nil {
		return false
	}
	key := from.String()
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.addrs) > maxPeers {
		t.addrs = make(map[string]*net.UDPAddr, 16)
	}
	if _, ok := t.addrs[key]; ok {
		return false
	}
	addr := *from
	t.addrs[key] = &addr
	return true
}

// RegisterRoute 注册"目的 MAC -> 对端地址"的帧路由。
// 调用方（client）在打洞成功后按候选回查每个对端的 MAC 逐个注册，
// 使数据帧能按目的 MAC 直送对应对端。
func (t *Tunnel) RegisterRoute(mac net.HardwareAddr, addr *net.UDPAddr) {
	if mac == nil || addr == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.routes == nil {
		t.routes = make(map[string]*net.UDPAddr)
	}
	a := *addr
	t.routes[mac.String()] = &a
}

// KnownAddr 该地址是否已验证可达（收到过它的包）。
func (t *Tunnel) KnownAddr(addr *net.UDPAddr) bool {
	if addr == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.addrs[addr.String()]
	return ok
}

// HasRoute 是否存在该目的 MAC 的直连路由。
func (t *Tunnel) HasRoute(mac net.HardwareAddr) bool {
	if mac == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.routes[mac.String()]
	return ok
}

// Routes 返回 MAC -> 地址 的路由快照（管理面诊断用）。
func (t *Tunnel) Routes() map[string]string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]string, len(t.routes))
	for k, v := range t.routes {
		out[k] = v.String()
	}
	return out
}

// Peers 返回全部已验证对端地址快照（管理面诊断与保活用）。
func (t *Tunnel) Peers() []*net.UDPAddr {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]*net.UDPAddr, 0, len(t.addrs))
	for _, v := range t.addrs {
		out = append(out, v)
	}
	return out
}

// startKeepalive 起一个保活发送器：隧道存活期间周期向全部已验证对端发探测包。
// 既维持 NAT 映射新鲜度，也保证"先收到对端探测"一侧在停发后，
// 对端仍能收到本侧的探测从而完成双向打通。
func (t *Tunnel) startKeepalive() {
	go func() {
		ticker := time.NewTicker(keepaliveInterval)
		defer ticker.Stop()
		for {
			select {
			case <-t.done:
				return
			case <-ticker.C:
				// 本侧刚发过真实数据帧时无需再发保活探测：
				// 数据流量本身就在持续刷新 NAT 映射。
				if tx := t.LastTxTime(); !tx.IsZero() && time.Since(tx) < keepaliveInterval {
					continue
				}
				for _, peer := range t.Peers() {
					t.writeMu.Lock()
					t.conn.WriteToUDP(probeMagic, peer)
					t.writeMu.Unlock()
				}
			}
		}
	}()
}

// shouldReplyProbe 按来源地址对探测回复做速率限制。
// 距上次向该来源回复不足 probeReplyMinInterval 时不再回复，
// 断开"回复触发回复"的乒乓自激，同时把无鉴权反射放大的幅度
// 压到 1 包/秒/来源以下。条目超限时整体重置，避免内存被撑爆。
func (t *Tunnel) shouldReplyProbe(from *net.UDPAddr) bool {
	key := from.String()
	t.probeReplyMu.Lock()
	defer t.probeReplyMu.Unlock()
	if len(t.probeReply) > probeReplyMaxSources {
		t.probeReply = make(map[string]time.Time, 16)
	}
	if last, ok := t.probeReply[key]; ok && time.Since(last) < probeReplyMinInterval {
		return false
	}
	t.probeReply[key] = time.Now()
	return true
}

func (t *Tunnel) peerAddrOf() *net.UDPAddr {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.peerAddr
}

// Read 读取一个数据帧（阻塞）。隧道未打通或已关闭时返回错误。
// 保活探测帧会被吸收（跳过），不会作为 L2 数据返回；
// 任何收包来源都会被记为可达对端（addrs），供保活与路由学习。
func (t *Tunnel) Read() ([]byte, error) {
	for {
		buf := make([]byte, maxUDPFrame)
		n, from, err := t.conn.ReadFromUDP(buf)
		t.touchRx()
		if err != nil {
			return nil, err
		}
		frame := buf[:n]
		t.addPeer(from)
		if isProbe(frame) {
			// disco 响应：对端打洞/保活探测原路回一个 probe，
			// 供对方 ping 命令测直连 RTT；不污染数据面。
			// 回复按来源限速，否则"收 probe 回 probe"会让双方
			// 互答回包形成乒乓自激（实测 ~27k pps 空转风暴）。
			if t.shouldReplyProbe(from) {
				t.writeMu.Lock()
				_, _ = t.conn.WriteToUDP(probeMagic, from)
				t.writeMu.Unlock()
			}
			continue
		}
		return frame, nil
	}
}

// SetReadDeadline 设置后续 Read 的截止时间（用于测试与超时控制）。
func (t *Tunnel) SetReadDeadline(dl time.Time) error {
	return t.conn.SetReadDeadline(dl)
}

// touchRx 在 Read 收包时刷新最近收包时间（供空闲监测）。
func (t *Tunnel) touchRx() {
	t.lastRxMu.Lock()
	t.lastRx = time.Now()
	t.lastRxMu.Unlock()
}

func (t *Tunnel) touchTx() {
	t.lastTxMu.Lock()
	t.lastTx = time.Now()
	t.lastTxMu.Unlock()
}

// LastRxTime 返回最近一次收到数据的时间（未收发过返回零值）。
func (t *Tunnel) LastRxTime() time.Time {
	t.lastRxMu.Lock()
	defer t.lastRxMu.Unlock()
	return t.lastRx
}

// LastTxTime 返回最近一次发出数据的时间（未发过返回零值）。
func (t *Tunnel) LastTxTime() time.Time {
	t.lastTxMu.Lock()
	defer t.lastTxMu.Unlock()
	return t.lastTx
}

// Write 按帧目的 MAC 路由发送数据帧。
// 未注册该 MAC 的直连路由时返回 ErrNoRoute——调用方应改走 TCP 中继，
// 且不必断开隧道（其余对端不受影响）。
func (t *Tunnel) Write(frame []byte) error {
	f, err := ethernet.ParseFrame(frame)
	if err != nil {
		return fmt.Errorf("vrndis/p2p: bad frame: %w", err)
	}
	t.mu.Lock()
	addr, ok := t.routes[f.Dst.String()]
	t.mu.Unlock()
	if !ok {
		return ErrNoRoute
	}
	if len(frame) > maxUDPFrame {
		return errors.New("vrndis/p2p: frame too large")
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	_, err = t.conn.WriteToUDP(frame, addr)
	if err == nil {
		t.touchTx()
	}
	return err
}

// Addr 返回本地 UDP 地址。
func (t *Tunnel) Addr() net.Addr { return t.conn.LocalAddr() }

// Peer 返回当前主对端地址（打洞窗口内首个回包来源）。
func (t *Tunnel) Peer() *net.UDPAddr { return t.peerAddrOf() }

// Close 关闭隧道并释放 UDP 连接。可重复调用。
func (t *Tunnel) Close() error {
	t.closeOnce.Do(func() { close(t.done) })
	return t.conn.Close()
}

// Done 隧道关闭时关闭（可用于空闲/断线监测驱动回退）。
func (t *Tunnel) Done() <-chan struct{} { return t.done }

// AttachIdleMonitor 起一个空闲监视：超过 idle 未收到任何数据报则关闭隧道并返回 ErrIdleTimeout。
// 返回的 cancel 可停止监视器。用于把打穿但失活的隧道回退到服务器中继。
// 空闲判定基于任意对端的最近收包时间：只要还有一个对端在通信，整条
// 隧道（共享 socket）就保持存活。
func (t *Tunnel) AttachIdleMonitor(idle time.Duration, logger *log.Logger) (cancel func()) {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(idle / 4)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.done:
				return
			case <-ticker.C:
			}
			t.lastRxMu.Lock()
			last := t.lastRx
			t.lastRxMu.Unlock()
			if !last.IsZero() && time.Since(last) > idle {
				logger.Printf("vrndis/p2p: tunnel idle timeout, closing")
				t.Close()
				return
			}
		}
	}()
	return func() { close(stop) }
}
