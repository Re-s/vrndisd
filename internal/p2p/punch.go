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
)

var errNoXORAddr = errors.New("vrndis/p2p: bad XOR-MAPPED-ADDRESS")

// maxUDPFrame 单个 UDP 数据报承载帧的最大字节数（覆盖标准以太网帧与常见 jumbo）。
const maxUDPFrame = 9000

// probeMagic 打洞/保活探测帧。隧道建立后 keepalive 会周期向对端发送该探测包
// 以刷新 NAT 映射。探测帧不是合法以太网帧，Tunnel.Read 会在数据面吸收（跳过）它，
// 避免把探测帧当作 L2 数据写入 TAP。
var probeMagic = []byte("vrndis-p2p-probe\x00\x01")

// isProbe 判断一个数据报是否为打洞/保活探测帧。
func isProbe(b []byte) bool {
	return len(b) == len(probeMagic) && string(b) == string(probeMagic)
}

// Tunnel 一个已打穿的双向 UDP 隧道。
//
// 对端地址取最后一次从候选集中收到数据报的来源（即对端实际公网映射）。
// 提供与 tap.Device 类似的 Read/Write 语义，可替换服务器中继数据面。
type Tunnel struct {
	conn *net.UDPConn

	done      chan struct{}
	closeOnce sync.Once

	writeMu  sync.Mutex
	mu       sync.Mutex
	peerAddr *net.UDPAddr

	// lastRx 记录最近一次收到数据的时间（受 lastRxMu 保护），
	// 供 AttachIdleMonitor 轮询判断隧道是否空闲；不要再读 socket。
	lastRx   time.Time
	lastRxMu sync.Mutex
}

// Punch 向 peerCandidates 打洞：持续向候选地址发送探测包，并等待收到对端探测。
// 一旦收到来自任一候选来源的数据报，即视为双向打通（对端已向我们发包、我们已向其发包），
// 返回已建立的 Tunnel。若在 ctx 截止前未打通，返回 ErrPunchTimeout。
func Punch(ctx context.Context, localPort int, peerCandidates []*net.UDPAddr) (*Tunnel, error) {
	local := &net.UDPAddr{IP: net.IPv4zero, Port: localPort}
	conn, err := net.ListenUDP("udp", local)
	if err != nil {
		return nil, fmt.Errorf("vrndis/p2p: listen punch: %w", err)
	}
	return punchConn(ctx, conn, peerCandidates)
}

// punchConn 在已绑定的 UDP 连接上打洞。conn 交由 Tunnel 持有，打通后由调用方关闭。
// 供测试在并发场景下显式绑定端口、避免端口复用竞态。
// PunchConn  UDP conn(STUN Discover )
//
//	Tunnel ;ctx  ErrPunchTimeout
//
// ,conn ;,
// ctx ,conn.Close()
func PunchConn(ctx context.Context, conn *net.UDPConn, peerCandidates []*net.UDPAddr) (*Tunnel, error) {
	return punchConn(ctx, conn, peerCandidates)
}

func punchConn(ctx context.Context, conn *net.UDPConn, peerCandidates []*net.UDPAddr) (*Tunnel, error) {
	// lastRx  ,
	//  AttachIdleMonitor
	//  Read  touchRx  .
	t := &Tunnel{conn: conn, done: make(chan struct{}), lastRx: time.Now()}

	dbg := log.New(os.Stderr, "punchdbg(local="+conn.LocalAddr().String()+"): ", log.LstdFlags|log.Lmicroseconds)
	dbg.Printf("start, candidates=%v", peerCandidates)

	sentOnce := false
	// 持续向候选地址发探测（间隔可控），直到打通或 ctx 结束。
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			conn.Close()
			return nil, ErrPunchTimeout
		case <-ticker.C:
			for _, cand := range peerCandidates {
				if cand == nil {
					continue
				}
				if _, werr := conn.WriteToUDP(probeMagic, cand); werr == nil {
					sentOnce = true
				}
			}
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
		t.setPeer(from)
		t.startKeepalive()
		dbg.Printf("OK peer=%v", from)
		//  punch  50ms  deadline,
		//  deadline  Tunnel.Read  EOF.
		_ = conn.SetReadDeadline(time.Time{})
		return t, nil
	}
}

func (t *Tunnel) setPeer(from *net.UDPAddr) {
	t.mu.Lock()
	t.peerAddr = from
	t.mu.Unlock()
}

// startKeepalive 起一个保活发送器：隧道存活期间周期向对端发探测包。
// 既维持 NAT 映射新鲜度，也保证"先收到对端探测"一侧在停发后，
// 对端仍能收到本侧的探测从而完成双向打通。
func (t *Tunnel) startKeepalive() {
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-t.done:
				return
			case <-ticker.C:
				peer := t.Peer()
				if peer == nil {
					continue
				}
				t.writeMu.Lock()
				t.conn.WriteToUDP(probeMagic, peer)
				t.writeMu.Unlock()
			}
		}
	}()
}

func (t *Tunnel) peerAddrOf() *net.UDPAddr {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.peerAddr
}

// Read 读取一个数据帧（阻塞）。隧道未打通或已关闭时返回错误。
// 保活探测帧会被吸收（跳过），不会作为 L2 数据返回。
func (t *Tunnel) Read() ([]byte, error) {
	for {
		buf := make([]byte, maxUDPFrame)
		n, _, err := t.conn.ReadFromUDP(buf)
		t.touchRx()
		if err != nil {
			return nil, err
		}
		frame := buf[:n]
		if isProbe(frame) {
			continue // 吸收保活探测，不污染数据面
		}
		return frame, nil
	}
}

// SetReadDeadline 设置后续 Read 的截止时间（用于测试与超时控制）。
// touchRx  Read
func (t *Tunnel) touchRx() {
	t.lastRxMu.Lock()
	t.lastRx = time.Now()
	t.lastRxMu.Unlock()
}

func (t *Tunnel) SetReadDeadline(dl time.Time) error {
	return t.conn.SetReadDeadline(dl)
}

// Write 发送一个数据帧到对端（未设置对端时错误）。
func (t *Tunnel) Write(frame []byte) error {
	peer := t.peerAddrOf()
	if peer == nil {
		return errors.New("vrndis/p2p: no peer set")
	}
	if len(frame) > maxUDPFrame {
		return errors.New("vrndis/p2p: frame too large")
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	_, err := t.conn.WriteToUDP(frame, peer)
	return err
}

// Addr 返回本地 UDP 地址。
func (t *Tunnel) Addr() net.Addr { return t.conn.LocalAddr() }

// Peer 返回当前对端地址。
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
// AttachIdleMonitor : idle  ErrIdleTimeout
//
//	cancel
//
//	lastRx ( Tunnel.Read ),
//	ReadFromUDP : Tunnel.Read
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
