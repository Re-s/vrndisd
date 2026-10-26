// Package netcheck 探测本机 NAT 环境与 UDP 可达性（P2P 打洞能力前置评估）。
//
// 机制对标 Tailscale netcheck：同一 UDP socket 向多个 STUN 目的地发送
// Binding Request，比对各目的地返回的 XOR-MAPPED-ADDRESS：
//   - 端口随目的地变化（MappingVariesByDest）→ 对称 NAT，
//     双向打洞大概率失败，流量将走服务器中继
//   - 端口保持一致 → cone 家族（full/restricted），打洞通常可行
//   - IP 变化 → 多出口/运营商级 NAT
//
// 同服务器双端口（:5478/:5479）只能识别"端口随目的地变"；跨 IP 差异
// 需额外指定第三方 STUN（-extra-stun）。
package netcheck

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/Re-s/vrndisd/internal/stun/protocol"
)

// maxRetries 单个目的地重试次数（提高 NAT 丢包下的成功率）。
const maxRetries = 2

// stunTimeout 单次 STUN 请求响应超时。
const stunTimeout = 1 * time.Second

// Result netcheck 报告。
type Result struct {
	UDPOK               bool         // UDP 出站可用（至少一个目的地应答）
	Primary             *net.UDPAddr // 主 STUN（服务器:5478）看到的映射
	PrimaryRTT          time.Duration
	Secondary           *net.UDPAddr // 第二 STUN（服务器:5479）
	SecondaryRTT        time.Duration
	Extra               *net.UDPAddr // 可选第三方 STUN（-extra-stun）
	ExtraRTT            time.Duration
	MappingVariesByDest bool   // 映射端口/IP 随目的地变化（对称 NAT 指征）
	NATType             string // full-cone-like / symmetric / multi-exit / udp-blocked / unknown
	HolepunchProspect   string // 打洞预期：容易 / 困难（回退中继） / 不可行 / 不确定
}

// probe 单个目的地的探测产物。
type probe struct {
	addr *net.UDPAddr
	rtt  time.Duration
	err  error
}

// Run 执行 netcheck。primary 必填（服务器 STUN 主端口），secondary 为
// 同服务器第二端口，extra 为可选第三方 STUN。共享 socket 保证映射可比对。
func Run(ctx context.Context, primary, secondary, extra string) (*Result, error) {
	if primary == "" {
		return nil, errors.New("github.com/Re-s/vrndisd/netcheck: primary STUN address is required")
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, fmt.Errorf("github.com/Re-s/vrndisd/netcheck: listen: %w", err)
	}
	defer conn.Close()

	dests := []string{primary}
	if secondary != "" {
		dests = append(dests, secondary)
	}
	if extra != "" {
		dests = append(dests, extra)
	}

	// 整体超时：每目的地最多 (maxRetries+1) 个 stunTimeout。
	runCtx, cancel := context.WithTimeout(ctx, stunTimeout*time.Duration(len(dests)*(maxRetries+1)))
	defer cancel()

	probes := make(map[string]*probe, len(dests))
	for _, d := range dests {
		probes[d] = probeDest(runCtx, conn, d)
	}
	return buildResult(probes, dests), nil
}

// probeDest 向单一目的地探测（共享 conn，串行调用）。
func probeDest(ctx context.Context, conn *net.UDPConn, dest string) *probe {
	raddr, err := net.ResolveUDPAddr("udp", dest)
	if err != nil {
		return &probe{err: fmt.Errorf("resolve %s: %w", dest, err)}
	}
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if ctx.Err() != nil {
			break
		}
		tx := makeTx()
		req := buildBinding(tx)
		dl := time.Now().Add(stunTimeout)
		if d, ok := ctx.Deadline(); ok && d.Before(dl) {
			dl = d
		}
		_ = conn.SetDeadline(dl)
		start := time.Now()
		if _, err := conn.WriteToUDP(req, raddr); err != nil {
			lastErr = err
			continue
		}
		buf := make([]byte, 2048)
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			lastErr = err
			continue // 读超时：重试
		}
		rtt := time.Since(start)
		msg, err := protocol.Parse(buf[:n])
		if err != nil || msg.TransactionID != tx {
			lastErr = errors.New("unexpected response")
			continue
		}
		if msg.Type != protocol.BindingResponse {
			lastErr = fmt.Errorf("stun type %d", msg.Type)
			continue
		}
		raw, ok := msg.Attributes[protocol.AttrXORMappedAddr]
		if !ok {
			lastErr = errors.New("missing XOR-MAPPED-ADDRESS")
			continue
		}
		addr, err := protocol.DecodeXORMapped(raw)
		if err != nil {
			lastErr = err
			continue
		}
		return &probe{addr: addr, rtt: rtt}
	}
	if lastErr == nil {
		lastErr = ctx.Err()
	}
	return &probe{err: fmt.Errorf("%s: %v", dest, lastErr)}
}

// buildResult 汇总各目的地映射并做 NAT 评估。
func buildResult(probes map[string]*probe, dests []string) *Result {
	r := &Result{}
	order := []*net.UDPAddr{nil, nil, nil} // primary/secondary/extra 映射
	rtts := make([]time.Duration, 3)
	for i, d := range dests {
		p := probes[d]
		if p == nil || p.err != nil {
			continue
		}
		order[i] = p.addr
		rtts[i] = p.rtt
	}
	r.Primary, r.PrimaryRTT = order[0], rtts[0]
	if len(dests) > 1 {
		r.Secondary, r.SecondaryRTT = order[1], rtts[1]
	}
	if len(dests) > 2 {
		r.Extra, r.ExtraRTT = order[2], rtts[2]
	}

	var addrs []*net.UDPAddr
	for _, a := range order {
		if a != nil {
			addrs = append(addrs, a)
		}
	}
	if len(addrs) == 0 {
		r.NATType = "udp-blocked"
		r.HolepunchProspect = "不可行（UDP 出站被阻断，只能走服务器中继）"
		return r
	}
	r.UDPOK = true

	if len(addrs) < 2 {
		r.NATType = "unknown"
		r.HolepunchProspect = "不确定（仅单个目的地应答，无法比对映射）"
		return r
	}

	// 比对所有成功目的地的映射 IP 与端口。
	first := addrs[0]
	portSame, ipSame := true, true
	for _, a := range addrs[1:] {
		if a.Port != first.Port {
			portSame = false
		}
		if !a.IP.Equal(first.IP) {
			ipSame = false
		}
	}
	switch {
	case !ipSame:
		r.MappingVariesByDest = true
		r.NATType = "multi-exit"
		r.HolepunchProspect = "困难（出口 IP 随目的地变化，多出口/运营商 NAT，通常回退中继）"
	case !portSame:
		r.MappingVariesByDest = true
		r.NATType = "symmetric"
		r.HolepunchProspect = "困难（对称 NAT：映射端口随目的地变化，双向打洞多失败）"
	default:
		r.NATType = "full-cone-like"
		r.HolepunchProspect = "容易（映射端口跨目的地保持，cone 家族打洞通常成功）"
	}
	return r
}

// makeTx 生成 12 字节事务 ID。
func makeTx() [12]byte {
	var tx [12]byte
	_, _ = rand.Read(tx[:])
	return tx
}

// buildBinding 构造无属性的 Binding Request。
func buildBinding(tx [12]byte) []byte {
	req := make([]byte, 20)
	binary.BigEndian.PutUint16(req[0:], uint16(protocol.BindingRequest))
	binary.BigEndian.PutUint16(req[2:], 0)
	binary.BigEndian.PutUint32(req[4:], protocol.MagicCookie)
	copy(req[8:20], tx[:])
	return req
}
