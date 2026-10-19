package server

import (
	"net"
	"testing"
)

func mustIP(t *testing.T, s string) net.IP {
	t.Helper()
	ip := net.ParseIP(s)
	if ip == nil {
		t.Fatalf("bad ip %q", s)
	}
	return ip
}

func TestIsPublicCandidate(t *testing.T) {
	cases := map[string]bool{
		"1.2.3.4:5678":     true,  // 公网 v4
		"[2600:1::1]:5678": true,  // 公网 v6
		"192.168.1.5:5678": false, // RFC1918
		"10.0.0.5:5678":    false,
		"100.64.0.1:5678":  false, // CGNAT
		"[fc00::1]:5678":   false, // ULA
		"[fe80::1]:5678":   false, // link-local
		"127.0.0.1:5678":   false, // loopback
		"1.2.3.4":          false, // 无端口
		"not-an-ip:5678":   false,
	}
	for in, want := range cases {
		if got := isPublicCandidate(in); got != want {
			t.Errorf("isPublicCandidate(%q)=%v want %v", in, got, want)
		}
	}
}

func TestCandidateEgress(t *testing.T) {
	got := candidateEgress([]string{"1.2.3.4:1"})
	if !got.Equal(mustIP(t, "1.2.3.4")) {
		t.Fatalf("got %v", got)
	}
	// 跳过私网取首个公网
	got = candidateEgress([]string{"192.168.1.5:1", "1.2.3.4:2"})
	if !got.Equal(mustIP(t, "1.2.3.4")) {
		t.Fatalf("got %v", got)
	}
	// 只有私网 → 无出口
	if got := candidateEgress([]string{"192.168.1.5:1"}); got != nil {
		t.Fatalf("want nil, got %v", got)
	}
	// v6 不作为 v4 出口
	if got := candidateEgress([]string{"[2600::1]:1", "5.6.7.8:2"}); !got.Equal(mustIP(t, "5.6.7.8")) {
		t.Fatalf("got %v", got)
	}
	if got := candidateEgress(nil); got != nil {
		t.Fatalf("want nil, got %v", got)
	}
}

// candidatesFor 是内网候选外泄的安全红线：
// 内网候选（私网 host 候选）只能下发给同公网出口的 peer。
func TestCandidatesForSameEgressOnly(t *testing.T) {
	provider := &peerInfo{
		candidates: []string{"1.2.3.4:100", "192.168.1.5:200"},
		egressIP:   mustIP(t, "1.2.3.4"),
	}
	// 同出口：公网 + 内网候选都下发
	receiver := &peerInfo{egressIP: mustIP(t, "1.2.3.4")}
	got := candidatesFor(receiver, provider)
	if len(got) != 2 {
		t.Fatalf("same egress: want 2 cands, got %v", got)
	}
	// 异出口：只有公网候选
	receiver = &peerInfo{egressIP: mustIP(t, "9.9.9.9")}
	got = candidatesFor(receiver, provider)
	if len(got) != 1 || got[0] != "1.2.3.4:100" {
		t.Fatalf("other egress: want only public, got %v", got)
	}
	// 接收方无 srflx 出口（nil）：保守只给公网
	receiver = &peerInfo{egressIP: nil}
	got = candidatesFor(receiver, provider)
	if len(got) != 1 || got[0] != "1.2.3.4:100" {
		t.Fatalf("nil receiver egress: want only public, got %v", got)
	}
	// 提供者无出口（只有 LAN 候选）：任何接收方都不应拿到内网候选
	lanOnly := &peerInfo{candidates: []string{"192.168.1.5:200"}, egressIP: nil}
	if got := candidatesFor(&peerInfo{egressIP: mustIP(t, "1.2.3.4")}, lanOnly); len(got) != 0 {
		t.Fatalf("provider without egress must not leak lan cand, got %v", got)
	}
	if got := candidatesFor(&peerInfo{egressIP: nil}, lanOnly); len(got) != 0 {
		t.Fatalf("both nil egress must not exchange lan cand, got %v", got)
	}
	// 全公网候选：任何接收方都全量
	allPublic := &peerInfo{candidates: []string{"1.2.3.4:100", "[2600:1::1]:100"}, egressIP: mustIP(t, "1.2.3.4")}
	for _, r := range []*peerInfo{{egressIP: mustIP(t, "9.9.9.9")}, {egressIP: nil}} {
		if got := candidatesFor(r, allPublic); len(got) != 2 {
			t.Fatalf("public cands must go to all, got %v", got)
		}
	}
}

// TestCandidatesForNilReceiver nil 接收方（查询者未上报自己的候选）必须
// 只回公网候选且绝不 panic。
// 回归 2026-10-20 生产卡死事故：vrndisd peers 诊断客户端不先 P2PAdvertise
// 就发 PeerQuery，answerPeerQuery 把 s.peers[selfMAC] 的 nil 直接透传给
// candidatesFor，provider 持公网+LAN 混合候选时 receiver.egressIP 空指针
// 解引用 panic，服务器被冻结到只能 SIGKILL。
func TestCandidatesForNilReceiver(t *testing.T) {
	// 公网 + LAN 混合候选（真实 Android 客户端的典型上报形态）
	provider := &peerInfo{
		candidates: []string{"1.2.3.4:100", "192.168.1.5:200"},
		egressIP:   mustIP(t, "1.2.3.4"),
	}
	got := candidatesFor(nil, provider)
	if len(got) != 1 || got[0] != "1.2.3.4:100" {
		t.Fatalf("nil receiver: want only public cand, got %v", got)
	}
	// 提供者只有 LAN 候选（无出口）：nil 接收方应拿到空列表
	lanOnly := &peerInfo{candidates: []string{"192.168.1.5:200"}, egressIP: nil}
	if got := candidatesFor(nil, lanOnly); len(got) != 0 {
		t.Fatalf("nil receiver + lan-only provider: want empty, got %v", got)
	}
}
