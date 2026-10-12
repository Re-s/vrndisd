// Package p2p 实现 vrndis 的 P2P 直连（NAT 打洞 + 中继回退）。
//
// 组成：
//   - Discover：通过 STUN 服务器获知本地 NAT 映射后的公网地址。
//   - Punch：向对端候选地址打洞，双向打穿后返回已建立的 UDP 隧道；
//     超时返回 ErrPunchTimeout，调用方可回退到服务器中继。
//   - Tunnel：UDP 上的可靠字节流（复用一个内置帧头），带活跃心跳与空闲超时。
package p2p

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"

	"vrndis/internal/stun/protocol"
)

// ErrPunchTimeout 打洞超时，NAT 无法打穿，应回退服务器中继。
var ErrPunchTimeout = errors.New("vrndis/p2p: punch timeout, fall back to server relay")

// ErrIdleTimeout UDP 隧道空闲超时，链路被认为失效，应回退。
var ErrIdleTimeout = errors.New("vrndis/p2p: tunnel idle timeout")

// Discover 通过 STUN 服务器获知本地 NAT 映射后的公网地址（XOR-MAPPED-ADDRESS）。
// 返回的地址同时含公网 IP 与端口，供打洞时向对端通告。
// Discover binds a UDP socket, sends a STUN binding request from it, and
// returns the socket together with its server-reflexive address.
//
// The socket is deliberately left OPEN: the reflexive address is only valid
// for the exact socket it was learned on, so the caller must reuse this
// socket for NAT punching (see PunchConn). Closing it first would publish
// a candidate that points at a dead port and punching would never succeed.
func Discover(ctx context.Context, stunServer string, localPort int) (*net.UDPConn, *net.UDPAddr, error) {
	saddr, err := net.ResolveUDPAddr("udp", stunServer)
	if err != nil {
		return nil, nil, fmt.Errorf("vrndis/p2p: resolve stun %q: %w", stunServer, err)
	}
	local, err := net.ResolveUDPAddr("udp", fmt.Sprintf(":%d", localPort))
	if err != nil {
		return nil, nil, err
	}
	conn, err := net.ListenUDP("udp", local)
	if err != nil {
		return nil, nil, fmt.Errorf("vrndis/p2p: listen: %w", err)
	}

	// 构造 Binding Request。
	req := make([]byte, 20)
	binary.BigEndian.PutUint16(req[0:], uint16(protocol.BindingRequest))
	binary.BigEndian.PutUint16(req[2:], 0) // 无属性
	binary.BigEndian.PutUint32(req[4:], protocol.MagicCookie)
	var tx [12]byte
	if _, err := random(tx[:]); err != nil {
		return nil, nil, err
	}
	copy(req[8:20], tx[:])

	// 读截止时间取 ctx 截止与默认 STUN 超时的较近者，保证 ctx 取消能尽快返回。
	dl := time.Now().Add(500 * time.Millisecond)
	if d, ok := ctx.Deadline(); ok && d.Before(dl) {
		dl = d
	}
	_ = conn.SetDeadline(dl)
	if _, err := conn.WriteToUDP(req, saddr); err != nil {
		return nil, nil, fmt.Errorf("vrndis/p2p: send stun: %w", err)
	}
	buf := make([]byte, 65535)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			// 命中 ctx 取消则报取消，否则读超时。
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			default:
			}
			return nil, nil, fmt.Errorf("vrndis/p2p: stun read: %w", err)
		}
		msg, err := protocol.Parse(buf[:n])
		if err != nil || msg.TransactionID != tx {
			continue // 忽略无关/重传的响应
		}
		if msg.Type != protocol.BindingResponse {
			return nil, nil, fmt.Errorf("vrndis/p2p: unexpected stun type %d", msg.Type)
		}
		raw, ok := msg.Attributes[protocol.AttrXORMappedAddr]
		if !ok {
			return nil, nil, errors.New("vrndis/p2p: stun response missing XOR-MAPPED-ADDRESS")
		}
		addr, err := decodeXORMapped(raw)
		if err != nil {
			conn.Close()
			return nil, nil, err
		}
		return conn, addr, nil
	}
}
