package main

import (
	"net"
	"testing"
	"time"
)

// echoUDP 起一个 disco 响应器（模拟隧道 PunchConn 的 Read 探测回包）。
func echoUDP(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	go func() {
		buf := make([]byte, 64)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = conn.WriteToUDP(buf[:n], from)
		}
	}()
	return conn.LocalAddr().String()
}

// TestDiscoProbeMultiPrefersReachable - 公网候选不可达（CGNAT 无 hairpin）
// 时，多候选探测必须命中可达的 LAN 候选，而不是整体判定"直连无响应"。
// 候选顺序故意把不可达公网放前面（主候选优先下发），验证并发不偏心。
func TestDiscoProbeMultiPrefersReachable(t *testing.T) {
	lan := echoUDP(t)
	pub := "127.0.0.1:1" // 无监听：不可达
	rtt, hit := discoProbeMulti([]string{pub, lan}, 2*time.Second)
	if hit != lan {
		t.Fatalf("应命中可达候选 %s，得到 %s（rtt=%v）", lan, hit, rtt)
	}
	if rtt <= 0 || rtt > time.Second {
		t.Fatalf("RTT 异常: %v", rtt)
	}
}

// TestDiscoProbeMultiEmpty - 空列表立即返回空命中。
func TestDiscoProbeMultiEmpty(t *testing.T) {
	rtt, hit := discoProbeMulti(nil, 100*time.Millisecond)
	if hit != "" || rtt != 0 {
		t.Fatalf("空列表应返回空命中: rtt=%v hit=%q", rtt, hit)
	}
}

// TestDiscoProbeMultiAllUnreachable - 全部不可达时在等待窗口内返回空命中。
func TestDiscoProbeMultiAllUnreachable(t *testing.T) {
	start := time.Now()
	_, hit := discoProbeMulti([]string{"127.0.0.1:1", "127.0.0.1:2"}, 500*time.Millisecond)
	if hit != "" {
		t.Fatalf("全部不可达应空命中: %s", hit)
	}
	if el := time.Since(start); el > 1500*time.Millisecond {
		t.Fatalf("超时等待过长: %v", el)
	}
}
