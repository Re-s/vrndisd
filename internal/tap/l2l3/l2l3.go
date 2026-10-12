// Package l2l3 为 wintun（三层 TUN 驱动）提供二层以太网语义的适配层。
//
// vrndis-go 数据面是二层以太网帧透传；wintun 只收发裸 IPv4/IPv6 包，
// 不认 14 字节以太网头，也不承载 ARP。本包在 wintun 上方模拟出 L2 语义：
//
//   - 上行（WrapIP）：裸 IP 包 → 伪以太网帧（补 14 字节头）
//   - 下行（UnwrapIP）：以太网帧 → 裸 IP 包；ARP 帧被拦截，
//     若请求的是本机 IP 则构造 ARP Reply（由调用方发回服务器），
//     并顺带学习 Src IP → Src MAC 填充本地缓存
//
// 设计约束：所有函数对畸形输入**静默丢弃（返回 nil/nil）而非报错**——
// client.devToSock / sockToDev 会把 Read/Write 的错误当作数据面终止，
// 一帧垃圾数据不能杀死整个客户端进程。
package l2l3

import (
	"net"
	"sync"
	"time"

	"vrndis/internal/ethernet"
)

const (
	// arpTTL ARP 缓存条目存活时间。隧道场景下对端 MAC 稳定，TTL 仅作兜底。
	arpTTL = 5 * time.Minute
	// cacheCap ARP 缓存容量上限（防无界增长）。
	cacheCap = 1024
)

// Cache 是 IP → MAC 的 ARP 缓存，带 TTL 与容量上限。
// 并发安全（devToSock / sockToDev 两个方向都会访问）。
type Cache struct {
	mu    sync.RWMutex
	items map[string]cacheEntry
}

type cacheEntry struct {
	mac     net.HardwareAddr
	expires time.Time
}

// NewCache 创建空缓存。
func NewCache() *Cache {
	return &Cache{items: make(map[string]cacheEntry)}
}

// Set 记录 IP → MAC。
func (c *Cache) Set(ip net.IP, mac net.HardwareAddr) {
	if c == nil || ip == nil || mac == nil {
		return
	}
	key := ipKey(ip)
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.items) >= cacheCap {
		c.evictLocked()
	}
	c.items[key] = cacheEntry{mac: append([]byte(nil), mac...), expires: time.Now().Add(arpTTL)}
}

// Get 查询 IP → MAC，未命中或过期返回 nil。
func (c *Cache) Get(ip net.IP) net.HardwareAddr {
	if c == nil || ip == nil {
		return nil
	}
	key := ipKey(ip)
	c.mu.RLock()
	e, ok := c.items[key]
	c.mu.RUnlock()
	if !ok {
		return nil
	}
	if time.Now().After(e.expires) {
		c.mu.Lock()
		delete(c.items, key)
		c.mu.Unlock()
		return nil
	}
	return e.mac
}

// Len 返回当前条目数（测试/诊断用）。
func (c *Cache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}

// evictLocked 清掉过期条目；仍超容则清掉半数（简单兜底）。
// 调用方必须已持有写锁。
func (c *Cache) evictLocked() {
	now := time.Now()
	for k, e := range c.items {
		if now.After(e.expires) {
			delete(c.items, k)
		}
	}
	if len(c.items) >= cacheCap {
		i := 0
		for k := range c.items {
			delete(c.items, k)
			i++
			if i >= cacheCap/2 {
				break
			}
		}
	}
}

// ipKey 归一化缓存键：IPv4 统一用 4 字节形式，避免 16 字节与 4 字节混存。
func ipKey(ip net.IP) string {
	if v4 := ip.To4(); v4 != nil {
		return string(v4)
	}
	return string(ip)
}

// WrapIP 把 wintun 读出的裸 IP 包封装成完整以太网帧（上行方向）。
//
// 参数：
//
//	payload  裸 IP 包（wintun 读出）
//	srcMAC   本机被分配的 MAC（Assignment.MAC）
//	dstIP    目的 IP（从 payload 解析）
//	gatewayIP   网关 IP（Assignment.Gateway）
//	gatewayMAC  网关 MAC（Assignment.ServerMAC）
//
// dstMAC 三级决策：
//
//  1. 目的 IP == 网关 IP → 网关 MAC —— 覆盖 ping 网关、默认路由出网
//  2. ARP 缓存命中 → 对端 MAC（cache 可为 nil）
//  3. 未命中 → 广播 ff:ff:ff:ff:ff:ff（服务器交换机泛洪，正确接收端
//     在 IP 层接收，其余在 L2 丢弃；下行学习会逐步消除广播）
//
// EtherType 按 IP 版本取 0x0800 / 0x86DD。
// 非法 IP 包（非 4/6 版本号）静默丢弃返回 nil —— wintun 理论上只吐 IP 包。
func WrapIP(payload []byte, srcMAC net.HardwareAddr, dstIP, gatewayIP net.IP, gatewayMAC net.HardwareAddr, cache *Cache) []byte {
	if len(payload) == 0 || srcMAC == nil {
		return nil
	}
	var etherType uint16
	var srcIP, dst net.IP
	switch payload[0] >> 4 {
	case 4:
		etherType = ethernet.TypeIPv4
		if len(payload) < 20 {
			return nil
		}
		srcIP = net.IP(payload[12:16])
		dst = net.IP(payload[16:20])
	case 6:
		etherType = ethernet.TypeIPv6
		if len(payload) < 40 {
			return nil
		}
		srcIP = net.IP(payload[8:24])
		dst = net.IP(payload[24:40])
	default:
		return nil // 非 IP 包，wintun 不应产出；静默丢弃
	}

	// 三级 Dst MAC 决策。
	var dstMAC net.HardwareAddr
	switch {
	case gatewayIP != nil && gatewayMAC != nil && dst != nil && dst.Equal(gatewayIP):
		dstMAC = append([]byte(nil), gatewayMAC...)
	case cache != nil && dst != nil && cache.Get(dst) != nil:
		dstMAC = cache.Get(dst)
	default:
		dstMAC = append([]byte(nil), ethernet.Broadcast...)
	}

	_ = srcIP // 源 IP 由伪头 Src MAC 表达（本机 MAC 即源身份）
	return ethernet.BuildFrame(dstMAC, srcMAC, etherType, payload)
}

// UnwrapIP 把服务器下发的以太网帧还原成裸 IP 包（下行方向），
// 并完成 ARP 拦截/应答与缓存学习。
//
// 返回值：
//
//	ipPayload  剥掉 14 字节头后的裸 IP 包；非 IP 帧为 nil（不进 wintun）
//	arpReply   需要发回服务器的 ARP 应答帧（完整以太网帧）；无需应答为 nil
//
// 处理规则：
//
//   - ARP 帧：拦截。若 TPA == localIP 且为请求（who-has），用
//     ethernet.ARPReply 构造应答（SHA=localMAC, SPA=localIP），
//     同时学习请求方 SPA→SHA；ARP 应答帧同样学习 SHA→SPA。
//   - IPv4/IPv6 帧：校验载荷首字节 nibble 合法后剥头；
//     顺带学习 Src IP → Src MAC（本机 ARP 缓存随使用自动收敛）。
//   - 其他 EtherType（LLDP/STP 等）：静默丢弃。
//
// 畸形帧（长度不足、非法 nibble）静默丢弃，返回 (nil, nil)，不 panic。
func UnwrapIP(frame []byte, localIP net.IP, localMAC net.HardwareAddr, cache *Cache) ([]byte, []byte) {
	if len(frame) < ethernet.HeaderLen {
		return nil, nil
	}
	f, err := ethernet.ParseFrame(frame)
	if err != nil {
		return nil, nil
	}

	if f.EtherType == ethernet.TypeARP {
		arp, err := ethernet.ParseARP(f.Payload)
		if err != nil {
			return nil, nil
		}
		// 学习：请求方的 SPA→SHA（无论是否应答都学）
		if cache != nil && arp.SHA != nil && arp.SPA != nil {
			cache.Set(arp.SPA, arp.SHA)
		}
		// 应答条件：ARP 请求 + 请求的目标 IP 是本机
		if arp.Op == ethernet.ARPOpRequest && localIP != nil && localMAC != nil &&
			arp.TPA != nil && arp.TPA.Equal(localIP) {
			reply := ethernet.ARPReply(localMAC, localIP, f.Src, arp.SPA)
			return nil, reply
		}
		return nil, nil
	}

	if f.EtherType != ethernet.TypeIPv4 && f.EtherType != ethernet.TypeIPv6 {
		return nil, nil // 与隧道无关的二层协议，丢弃
	}
	if len(f.Payload) == 0 || (f.Payload[0]>>4) != 4 && (f.Payload[0]>>4) != 6 {
		return nil, nil // 载荷不是合法 IP 包，丢弃
	}

	// 学习：下行帧的 Src MAC 与 IP 载荷中的源 IP 的绑定。
	if cache != nil {
		var srcIP net.IP
		if f.EtherType == ethernet.TypeIPv4 && len(f.Payload) >= 20 {
			srcIP = net.IP(f.Payload[12:16])
		} else if f.EtherType == ethernet.TypeIPv6 && len(f.Payload) >= 40 {
			srcIP = net.IP(f.Payload[8:24])
		}
		if srcIP != nil && f.Src != nil && !ethernet.IsMulticast(f.Src) {
			cache.Set(srcIP, f.Src)
		}
	}
	return f.Payload, nil
}

// IsARP 判断帧是否为 ARP 帧。
func IsARP(frame []byte) bool {
	if len(frame) < ethernet.HeaderLen {
		return false
	}
	f, err := ethernet.ParseFrame(frame)
	return err == nil && f.EtherType == ethernet.TypeARP
}

// HandleARP 处理一个 ARP 帧，返回应发回的 ARP Reply 帧；不应答返回 nil。
// 它是 UnwrapIP 中 ARP 分支的独立入口，便于测试与复用。
func HandleARP(frame []byte, localIP net.IP, localMAC net.HardwareAddr) []byte {
	if !IsARP(frame) {
		return nil
	}
	f, err := ethernet.ParseFrame(frame)
	if err != nil {
		return nil
	}
	arp, err := ethernet.ParseARP(f.Payload)
	if err != nil {
		return nil
	}
	if arp.Op == ethernet.ARPOpRequest && localIP != nil && localMAC != nil &&
		arp.TPA != nil && arp.TPA.Equal(localIP) {
		return ethernet.ARPReply(localMAC, localIP, f.Src, arp.SPA)
	}
	return nil
}
