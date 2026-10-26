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
