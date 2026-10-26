// Package addr 实现服务器端虚拟地址池（IP + MAC 分配与回收）。
package addr

import (
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/Re-s/vrndisd/internal/ethernet"
)

// ErrExhausted 地址池耗尽。
var ErrExhausted = errors.New("github.com/Re-s/vrndisd/addr: address pool exhausted")

// ErrNotFree 指定复用的地址不在池内或已被占用（应回退到 Allocate）。
var ErrNotFree = errors.New("github.com/Re-s/vrndisd/addr: requested address not available")

// Lease 一次地址租约。
type Lease struct {
	IP       net.IP
	Prefix   int
	MAC      net.HardwareAddr
	Gateway  net.IP
	ClientID uint64
}

// Pool 线程安全的 IPv4 地址池。
type Pool struct {
	mu      sync.Mutex
	network *net.IPNet
	gateway net.IP
	prefix  int
	free    []net.IP // 队列：FIFO 分配；释放的地址插回队头优先复用
	leases  map[string]*Lease
	nextID  uint64
}

// New 创建地址池；gateway 必须位于 network 内。
func New(network, gateway string) (*Pool, error) {
	_, ipnet, err := net.ParseCIDR(network)
	if err != nil {
		return nil, fmt.Errorf("bad network %q: %w", network, err)
	}
	ip4 := ipnet.IP.To4()
	if ip4 == nil {
		return nil, errors.New("github.com/Re-s/vrndisd/addr: only IPv4 networks are supported")
	}
	ones, bits := ipnet.Mask.Size()
	if bits != 32 {
		return nil, errors.New("github.com/Re-s/vrndisd/addr: only IPv4 networks are supported")
	}
	gw := net.ParseIP(gateway).To4()
	if gw == nil || !ipnet.Contains(gw) {
		return nil, fmt.Errorf("gateway %q not inside %q", gateway, network)
	}

	p := &Pool{
		network: ipnet,
		gateway: gw,
		prefix:  ones,
		leases:  make(map[string]*Lease),
		nextID:  1,
	}
	// 可用主机地址 = 网络内除网关外的全部主机
	for ip := nextIP(ip4); ip != nil && ipnet.Contains(ip); ip = nextIP(ip) {
		if isBroadcast(ip, ipnet) || ip.Equal(gw) {
			continue
		}
		p.free = append(p.free, append(net.IP(nil), ip...))
	}
	return p, nil
}

// Allocate 分配一个地址；池耗尽返回 ErrExhausted。
func (p *Pool) Allocate() (*Lease, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.free) == 0 {
		return nil, ErrExhausted
	}
	// 取队头，保证 .2, .3, ... 顺序分配
	ip := p.free[0]
	p.free = p.free[1:]
	id := p.nextID
	p.nextID++
	l := &Lease{
		IP:       ip,
		Prefix:   p.prefix,
		MAC:      ethernet.MakeLocalMAC(id),
		Gateway:  p.gateway,
		ClientID: id,
	}
	p.leases[ip.String()] = l
	return l, nil
}

// AllocateSpecific 按指定的 IP 与 MAC 分配租约：客户端重连时复用原租约，
// 保证同一客户端的 IP/MAC 跨服务器重启保持稳定（MAC 不再随连接顺序漂移）。
//
// 约束：IP 必须在池内且当前空闲（被别的客户端占用时返回 ErrNotFree，
// 调用方应回退 Allocate）；nextID 推进到超过该 MAC 的编码值，尽量避免
// 后续 Allocate 生成相同 MAC。
func (p *Pool) AllocateSpecific(ip net.IP, mac net.HardwareAddr) (*Lease, error) {
	if len(mac) != 6 {
		return nil, errors.New("github.com/Re-s/vrndisd/addr: bad MAC length")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	ip4 := ip.To4()
	if ip4 == nil || !p.network.Contains(ip4) || isBroadcast(ip4, p.network) || ip4.Equal(p.gateway) {
		return nil, ErrNotFree
	}
	key := ip4.String()
	if _, busy := p.leases[key]; busy {
		return nil, ErrNotFree
	}
	idx := -1
	for i, f := range p.free {
		if f.Equal(ip4) {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, ErrNotFree
	}
	p.free = append(p.free[:idx], p.free[idx+1:]...)
	last := int(mac[5])
	if last >= int(p.nextID) {
		p.nextID = uint64(last) + 1
	}
	l := &Lease{
		IP:       append(net.IP(nil), ip4...),
		Prefix:   p.prefix,
		MAC:      append(net.HardwareAddr(nil), mac...),
		Gateway:  append(net.IP(nil), p.gateway...),
		ClientID: uint64(last),
	}
	p.leases[key] = l
	return l, nil
}

// Release 回收租约，地址立即可复用（插回队头，优先分配刚释放的）。
func (p *Pool) Release(ip net.IP) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := ip.String()
	if _, ok := p.leases[key]; ok {
		delete(p.leases, key)
		p.free = append([]net.IP{append(net.IP(nil), ip.To4()...)}, p.free...)
	}
}

// InUse 当前租约数。
func (p *Pool) InUse() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.leases)
}

// Free 剩余可用地址数。
func (p *Pool) Free() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.free)
}

// Prefix 网络前缀长度。
func (p *Pool) Prefix() int { return p.prefix }

// Gateway 网关地址。
func (p *Pool) Gateway() net.IP { return append(net.IP(nil), p.gateway...) }

// Contains 报告 ip 是否属于本隧道地址池。
func (p *Pool) Contains(ip net.IP) bool { return p.network.Contains(ip) }

func nextIP(ip net.IP) net.IP {
	next := append(net.IP(nil), ip.To4()...)
	for i := 3; i >= 0; i-- {
		next[i]++
		if next[i] != 0 {
			return next
		}
	}
	return nil
}

func isBroadcast(ip net.IP, n *net.IPNet) bool {
	ip4 := ip.To4()
	mask := n.Mask
	for i := 0; i < 4; i++ {
		if ip4[i]|mask[i] != 0xff {
			return false
		}
	}
	return true
}
