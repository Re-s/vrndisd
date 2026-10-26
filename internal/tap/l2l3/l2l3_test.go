// l2l3 跨平台单测。
//
// 覆盖 wintun 适配层的全部纯逻辑路径：
//
//  1. WrapIP → UnwrapIP 往返（IPv4 / IPv6）
//  2. Dst MAC 三级决策（网关 → 缓存命中 → 广播）
//  3. ARP 拦截：TPA == 本机 IP 时生成正确 Reply
//  4. ARP 不拦截：TPA 非本机 IP 时无 Reply 但学习请求方
//  5. ARP Reply 下行：无 Reply，学习 sender IP→MAC
//  6. 数据帧学习：下行 IPv4/IPv6 帧填充 Cache
//  7. 未知 EtherType（LLDP 0x88CC）：静默丢弃
//  8. 畸形帧：长度 < 14、非法 nibble，不 panic
//  9. Cache TTL：过期条目被清除
//  10. ARP 载荷 < 28 字节：不处理
//
// 测试与实现同包（package l2l3），以便直接操作 Cache 的
// 私有字段构造过期条目，验证 TTL 行为。
package l2l3

import (
	"bytes"
	"encoding/binary"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/Re-s/vrndisd/internal/ethernet"
)

// ---- 测试夹具 -------------------------------------------------------------

var (
	// 本机（客户端）身份。
	testLocalMAC = net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0x0c}
	testLocalIP  = net.IP{10, 77, 0, 3}
	// 网关（服务器）身份：ServerMAC 固定为 02:00:00:00:00:00。
	testGatewayMAC = net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0x00}
	testGatewayIP  = net.IP{10, 77, 0, 1}
	// 模拟的另一个客户端（对端）。
	testPeerMAC = net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0x0d}
	testPeerIP  = net.IP{10, 77, 0, 4}
)

// ipv4Packet 构造一个最小合法 IPv4 包（20 字节头 + 载荷）。
func ipv4Packet(src, dst net.IP, payload []byte) []byte {
	pkt := make([]byte, 20+len(payload))
	pkt[0] = 0x45 // 版本 4，IHL 5
	binary.BigEndian.PutUint16(pkt[2:4], uint16(20+len(payload)))
	pkt[8] = 64 // TTL
	pkt[9] = 1  // ICMP
	copy(pkt[12:16], src.To4())
	copy(pkt[16:20], dst.To4())
	copy(pkt[20:], payload)
	return pkt
}

// ipv6Packet 构造一个最小合法 IPv6 包（40 字节头 + 载荷）。
func ipv6Packet(src, dst net.IP, payload []byte) []byte {
	pkt := make([]byte, 40+len(payload))
	pkt[0] = 0x60 // 版本 6
	binary.BigEndian.PutUint16(pkt[4:6], uint16(len(payload)))
	pkt[6] = 58 // ICMPv6
	pkt[7] = 64 // hop limit
	copy(pkt[8:24], src.To16())
	copy(pkt[24:40], dst.To16())
	copy(pkt[40:], payload)
	return pkt
}

// arpRequestFrame 构造完整 ARP 请求帧（who-has tpa tell spa）。
func arpRequestFrame(sha net.HardwareAddr, spa, tpa net.IP) []byte {
	return ethernet.ARPRequest(sha, spa, tpa)
}

// ---- 1. WrapIP → UnwrapIP 往返 -------------------------------------------

func TestWrapUnwrapRoundTrip(t *testing.T) {
	cache := NewCache()

	t.Run("IPv4 往返 payload 完全一致", func(t *testing.T) {
		icmp := []byte{0x08, 0x00, 0x00, 0x00, 'h', 'e', 'l', 'l', 'o'}
		pkt := ipv4Packet(testLocalIP, testGatewayIP, icmp)

		frame := WrapIP(pkt, testLocalMAC, testGatewayIP, testGatewayIP, testGatewayMAC, cache)
		if frame == nil {
			t.Fatal("WrapIP 返回 nil")
		}
		if len(frame) != ethernet.HeaderLen+len(pkt) {
			t.Fatalf("帧长 = %d，期望 %d（14 + %d）", len(frame), ethernet.HeaderLen+len(pkt), len(pkt))
		}
		// 剥头：下行方向 localIP 为本机 IP。
		ipPayload, arpReply := UnwrapIP(frame, testLocalIP, testLocalMAC, cache)
		if arpReply != nil {
			t.Fatalf("数据帧不应产生 ARP Reply，得到 %v", arpReply)
		}
		if !bytes.Equal(ipPayload, pkt) {
			t.Fatalf("往返后 payload 不一致\n 得到 %x\n 期望 %x", ipPayload, pkt)
		}
	})

	t.Run("IPv6 往返 payload 完全一致", func(t *testing.T) {
		icmp := []byte{0x80, 0x00, 0x00, 0x00, 'v', '6'}
		pkt := ipv6Packet(testLocalIP, testPeerIP, icmp)

		frame := WrapIP(pkt, testLocalMAC, testPeerIP, testGatewayIP, testGatewayMAC, cache)
		if frame == nil {
			t.Fatal("WrapIP 返回 nil")
		}
		ipPayload, arpReply := UnwrapIP(frame, testLocalIP, testLocalMAC, cache)
		if arpReply != nil {
			t.Fatalf("数据帧不应产生 ARP Reply，得到 %v", arpReply)
		}
		if !bytes.Equal(ipPayload, pkt) {
			t.Fatalf("往返后 payload 不一致\n 得到 %x\n 期望 %x", ipPayload, pkt)
		}
	})
}

// ---- 2. Dst MAC 三级决策 ---------------------------------------------------

func TestWrapIPDstMACThreeTier(t *testing.T) {
	pkt := ipv4Packet(testLocalIP, testGatewayIP, []byte{0x08, 0x00})

	t.Run("第一级：目的 IP == 网关 IP → 网关 MAC", func(t *testing.T) {
		frame := WrapIP(pkt, testLocalMAC, testGatewayIP, testGatewayIP, testGatewayMAC, NewCache())
		f, err := ethernet.ParseFrame(frame)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(f.Dst, testGatewayMAC) {
			t.Fatalf("Dst = %v，期望网关 MAC %v", f.Dst, testGatewayMAC)
		}
		if !bytes.Equal(f.Src, testLocalMAC) {
			t.Fatalf("Src = %v，期望本机 MAC %v", f.Src, testLocalMAC)
		}
		if f.EtherType != ethernet.TypeIPv4 {
			t.Fatalf("EtherType = %#x，期望 %#x", f.EtherType, ethernet.TypeIPv4)
		}
	})

	t.Run("第二级：ARP 缓存命中 → 学到的对端 MAC", func(t *testing.T) {
		cache := NewCache()
		cache.Set(testPeerIP, testPeerMAC)
		peerPkt := ipv4Packet(testLocalIP, testPeerIP, []byte{0x08, 0x00})

		frame := WrapIP(peerPkt, testLocalMAC, testPeerIP, testGatewayIP, testGatewayMAC, cache)
		f, err := ethernet.ParseFrame(frame)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(f.Dst, testPeerMAC) {
			t.Fatalf("Dst = %v，期望缓存中的对端 MAC %v", f.Dst, testPeerMAC)
		}
	})

	t.Run("第三级：缓存未命中 → 广播", func(t *testing.T) {
		unknownPkt := ipv4Packet(testLocalIP, testPeerIP, []byte{0x08, 0x00})
		frame := WrapIP(unknownPkt, testLocalMAC, testPeerIP, testGatewayIP, testGatewayMAC, NewCache())
		f, err := ethernet.ParseFrame(frame)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(f.Dst, ethernet.Broadcast) {
			t.Fatalf("Dst = %v，期望广播 %v", f.Dst, ethernet.Broadcast)
		}
	})
}

// ---- 3. ARP 拦截：请求本机 IP → 生成正确 Reply ------------------------------

func TestARPRequestToUs(t *testing.T) {
	// 服务器网关询问 "who has 10.77.0.3? tell 10.77.0.1"
	req := arpRequestFrame(testGatewayMAC, testGatewayIP, testLocalIP)
	if !IsARP(req) {
		t.Fatal("IsARP 应识别 ARP 请求帧")
	}

	localIP, localMAC := net.IP{10, 77, 0, 3}, testLocalMAC
	ipPayload, arpReply := UnwrapIP(req, localIP, localMAC, NewCache())

	if ipPayload != nil {
		t.Fatalf("ARP 帧不应产出 IP payload，得到 %x", ipPayload)
	}
	if arpReply == nil {
		t.Fatal("请求本机 IP 的 ARP 请求必须生成 Reply")
	}

	// 解析 Reply 验证字段。
	f, err := ethernet.ParseFrame(arpReply)
	if err != nil {
		t.Fatalf("Reply 不是合法以太网帧: %v", err)
	}
	if f.EtherType != ethernet.TypeARP {
		t.Fatalf("Reply EtherType = %#x，期望 ARP", f.EtherType)
	}
	// Reply 是单播给请求方：Dst = 请求方 MAC（网关），Src = 本机 MAC。
	if !bytes.Equal(f.Dst, testGatewayMAC) {
		t.Fatalf("Reply Dst = %v，期望请求方 MAC %v", f.Dst, testGatewayMAC)
	}
	if !bytes.Equal(f.Src, localMAC) {
		t.Fatalf("Reply Src = %v，期望本机 MAC %v", f.Src, localMAC)
	}
	arp, err := ethernet.ParseARP(f.Payload)
	if err != nil {
		t.Fatalf("Reply ARP 载荷解析失败: %v", err)
	}
	if arp.Op != ethernet.ARPOpReply {
		t.Fatalf("ARP Op = %d，期望 %d（Reply）", arp.Op, ethernet.ARPOpReply)
	}
	if !bytes.Equal(arp.SHA, localMAC) || !arp.SPA.Equal(localIP) {
		t.Fatalf("Reply 发送方 = %v/%v，期望本机 %v/%v", arp.SHA, arp.SPA, localMAC, localIP)
	}
	if !bytes.Equal(arp.THA, testGatewayMAC) || !arp.TPA.Equal(testGatewayIP) {
		t.Fatalf("Reply 目标 = %v/%v，期望请求方 %v/%v", arp.THA, arp.TPA, testGatewayMAC, testGatewayIP)
	}

	// HandleARP 独立入口应产生同样结果。
	if reply2 := HandleARP(req, localIP, localMAC); !bytes.Equal(reply2, arpReply) {
		t.Fatalf("HandleARP 与 UnwrapIP 结果不一致:\n %x\n %x", reply2, arpReply)
	}
}

// ---- 4. ARP 不拦截：TPA 非本机 IP ------------------------------------------

func TestARPRequestNotToUs(t *testing.T) {
	// 网关询问别人的 IP："who has 10.77.0.4? tell 10.77.0.1"
	req := arpRequestFrame(testGatewayMAC, testGatewayIP, testPeerIP)

	cache := NewCache()
	ipPayload, arpReply := UnwrapIP(req, testLocalIP, testLocalMAC, cache)

	if ipPayload != nil || arpReply != nil {
		t.Fatalf("非本机 ARP 请求不应应答，得到 (%x, %x)", ipPayload, arpReply)
	}
	// 但请求方的 SPA→SHA 绑定应被学习。
	if mac := cache.Get(testGatewayIP); mac == nil || !bytes.Equal(mac, testGatewayMAC) {
		t.Fatalf("应学习请求方 %v→%v，缓存为 %v", testGatewayIP, testGatewayMAC, mac)
	}
}

// ---- 5. ARP Reply 下行：学习 sender IP→MAC ----------------------------------

func TestARPReplyLearned(t *testing.T) {
	// 对端回复我们的 ARP 请求：10.77.0.4 is-at 02:00:00:00:00:0d
	reply := ethernet.ARPReply(testPeerMAC, testPeerIP, testLocalMAC, testLocalIP)

	cache := NewCache()
	ipPayload, arpReply := UnwrapIP(reply, testLocalIP, testLocalMAC, cache)

	if ipPayload != nil || arpReply != nil {
		t.Fatalf("ARP Reply 不应再触发应答，得到 (%x, %x)", ipPayload, arpReply)
	}
	if mac := cache.Get(testPeerIP); mac == nil || !bytes.Equal(mac, testPeerMAC) {
		t.Fatalf("应学习 Reply 发送方 %v→%v，缓存为 %v", testPeerIP, testPeerMAC, mac)
	}
}

// ---- 6. 数据帧学习 ----------------------------------------------------------

func TestDataFrameLearning(t *testing.T) {
	t.Run("IPv4 帧学习 Src IP→Src MAC", func(t *testing.T) {
		pkt := ipv4Packet(testPeerIP, testLocalIP, []byte{0x08, 0x00, 'x'})
		frame := ethernet.BuildFrame(testLocalMAC, testPeerMAC, ethernet.TypeIPv4, pkt)

		cache := NewCache()
		ipPayload, _ := UnwrapIP(frame, testLocalIP, testLocalMAC, cache)
		if !bytes.Equal(ipPayload, pkt) {
			t.Fatal("IPv4 payload 剥头错误")
		}
		if mac := cache.Get(testPeerIP); mac == nil || !bytes.Equal(mac, testPeerMAC) {
			t.Fatalf("应学习 %v→%v，缓存为 %v", testPeerIP, testPeerMAC, mac)
		}
	})

	t.Run("IPv6 帧学习 Src IP→Src MAC", func(t *testing.T) {
		pkt := ipv6Packet(testPeerIP, testLocalIP, []byte{0x80})
		frame := ethernet.BuildFrame(testLocalMAC, testPeerMAC, ethernet.TypeIPv6, pkt)

		cache := NewCache()
		ipPayload, _ := UnwrapIP(frame, testLocalIP, testLocalMAC, cache)
		if !bytes.Equal(ipPayload, pkt) {
			t.Fatal("IPv6 payload 剥头错误")
		}
		if mac := cache.Get(testPeerIP); mac == nil || !bytes.Equal(mac, testPeerMAC) {
			t.Fatalf("应学习 %v→%v，缓存为 %v", testPeerIP, testPeerMAC, mac)
		}
	})

	t.Run("组播源 MAC 不学习", func(t *testing.T) {
		pkt := ipv4Packet(testPeerIP, testLocalIP, []byte{0x08})
		multicastSrc := net.HardwareAddr{0x01, 0x00, 0x5e, 0x00, 0x00, 0x01}
		frame := ethernet.BuildFrame(testLocalMAC, multicastSrc, ethernet.TypeIPv4, pkt)

		cache := NewCache()
		if _, _ = UnwrapIP(frame, testLocalIP, testLocalMAC, cache); cache.Len() != 0 {
			t.Fatalf("组播源不应被学习，缓存条目数 = %d", cache.Len())
		}
	})
}

// ---- 7. 未知 EtherType ------------------------------------------------------

func TestUnknownEtherTypeDropped(t *testing.T) {
	// 0x88CC = LLDP，与隧道无关。
	lldp := bytes.Repeat([]byte{0xAA}, 20)
	frame := ethernet.BuildFrame(testLocalMAC, testPeerMAC, 0x88CC, lldp)

	ipPayload, arpReply := UnwrapIP(frame, testLocalIP, testLocalMAC, NewCache())
	if ipPayload != nil || arpReply != nil {
		t.Fatalf("未知 EtherType 帧应被静默丢弃，得到 (%x, %x)", ipPayload, arpReply)
	}
	if IsARP(frame) {
		t.Fatal("IsARP 不应识别 LLDP 帧")
	}
}

// ---- 8. 畸形帧 ----------------------------------------------------------------

func TestMalformedFramesNoPanic(t *testing.T) {
	cases := map[string][]byte{
		"长度不足 14 字节":        {0x45, 0x00, 0x00},
		"空帧":                {},
		"IPv4 载荷 nibble 非法": ethernet.BuildFrame(testPeerMAC, testLocalMAC, ethernet.TypeIPv4, []byte{0x35, 0x00}), // 0x3 → 非法版本
		"IPv6 载荷 nibble 非法": ethernet.BuildFrame(testPeerMAC, testLocalMAC, ethernet.TypeIPv6, []byte{0x20}),       // 0x2 → 非法版本
	}
	for name, frame := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("畸形帧触发 panic: %v", r)
				}
			}()
			ipPayload, arpReply := UnwrapIP(frame, testLocalIP, testLocalMAC, NewCache())
			if ipPayload != nil || arpReply != nil {
				t.Fatalf("畸形帧应被丢弃，得到 (%x, %x)", ipPayload, arpReply)
			}
		})
	}

	t.Run("WrapIP 非法 IP 包", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("WrapIP 畸形输入触发 panic: %v", r)
			}
		}()
		bad := []byte{0x35, 0x00} // 非法版本号 nibble
		if f := WrapIP(bad, testLocalMAC, testPeerIP, testGatewayIP, testGatewayMAC, NewCache()); f != nil {
			t.Fatalf("非 IP 包应返回 nil，得到 %x", f)
		}
	})
}

// ---- 9. Cache TTL -----------------------------------------------------------

func TestCacheTTL(t *testing.T) {
	cache := NewCache()
	cache.Set(testPeerIP, testPeerMAC)
	if mac := cache.Get(testPeerIP); mac == nil {
		t.Fatal("刚写入的条目应命中")
	}

	// 同包测试：直接把条目过期时间改到过去，验证 Get 清除它。
	key := string(testPeerIP.To4())
	cache.mu.Lock()
	cache.items[key] = cacheEntry{mac: testPeerMAC, expires: time.Now().Add(-time.Second)}
	cache.mu.Unlock()

	if mac := cache.Get(testPeerIP); mac != nil {
		t.Fatalf("过期条目应未命中，得到 %v", mac)
	}
	cache.mu.RLock()
	_, still := cache.items[key]
	cache.mu.RUnlock()
	if still {
		t.Fatal("Get 后过期条目应被删除")
	}
}

func TestCacheEvict(t *testing.T) {
	cache := NewCache()
	for i := 0; i < cacheCap; i++ {
		cache.Set(net.IP{10, 77, 0, byte(i % 256)}, net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, byte(i % 256)})
	}
	// 再塞一条触发淘汰：条目数应回落到容量的一半以下。
	cache.Set(net.IP{10, 77, 1, 1}, net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0x01})
	if got := cache.Len(); got > cacheCap/2+1 {
		t.Fatalf("淘汰后条目数 = %d，期望 ≤ %d", got, cacheCap/2+1)
	}
}

func TestCacheConcurrent(t *testing.T) {
	cache := NewCache()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				ip := net.IP{10, 77, byte(g), byte(i % 256)}
				cache.Set(ip, testPeerMAC)
				_ = cache.Get(ip)
				_ = cache.Len()
			}
		}(g)
	}
	wg.Wait()
}

// ---- 10. ARP 载荷过短 -------------------------------------------------------

func TestARPTooShort(t *testing.T) {
	// 以太网头合法，但 ARP 载荷不足 28 字节 → 不处理、不 panic。
	short := ethernet.BuildFrame(testGatewayMAC, testLocalMAC, ethernet.TypeARP, []byte{0x00, 0x01, 0x08, 0x00})
	ipPayload, arpReply := UnwrapIP(short, testLocalIP, testLocalMAC, NewCache())
	if ipPayload != nil || arpReply != nil {
		t.Fatalf("过短 ARP 载荷应被丢弃，得到 (%x, %x)", ipPayload, arpReply)
	}
	if reply := HandleARP(short, testLocalIP, testLocalMAC); reply != nil {
		t.Fatalf("HandleARP 对过短载荷应返回 nil，得到 %x", reply)
	}
}

// ---- 补充：HandleARP / IsARP 边界 -------------------------------------------

func TestHandleARPAndIsARPEdges(t *testing.T) {
	t.Run("非 ARP 帧 HandleARP 返回 nil", func(t *testing.T) {
		pkt := ipv4Packet(testPeerIP, testLocalIP, []byte{0x08})
		frame := ethernet.BuildFrame(testPeerMAC, testLocalMAC, ethernet.TypeIPv4, pkt)
		if reply := HandleARP(frame, testLocalIP, testLocalMAC); reply != nil {
			t.Fatalf("非 ARP 帧不应答，得到 %x", reply)
		}
	})
	t.Run("短帧 IsARP 返回 false", func(t *testing.T) {
		if IsARP([]byte{0x08, 0x06}) {
			t.Fatal("短帧不应被识别为 ARP")
		}
	})
	t.Run("nil Cache 不 panic", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("nil cache 触发 panic: %v", r)
			}
		}()
		pkt := ipv4Packet(testPeerIP, testLocalIP, []byte{0x08})
		frame := ethernet.BuildFrame(testPeerMAC, testLocalMAC, ethernet.TypeIPv4, pkt)
		if _, _ = UnwrapIP(frame, testLocalIP, testLocalMAC, nil); false {
			t.Fatal("不可达")
		}
	})
}
