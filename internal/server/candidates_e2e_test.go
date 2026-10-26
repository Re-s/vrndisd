package server_test

// 多候选分发的端到端契约：内网候选只下发给同公网出口的 peer。
//
// 回归红线（候选多路化引入的安全/有效性契约）：
//   - 同出口（B/A 均经 1.2.3.4）：互发全部候选（srflx + LAN host）
//   - 异出口（C 经 9.9.9.9）：只收到公网候选，LAN 地址不得外泄
//   - Candidate 兼容字段 = 过滤后列表首条

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/Re-s/vrndisd/internal/ctrl"
	"github.com/Re-s/vrndisd/internal/proto"
)

func TestCandidateDistributionFilteredByEgress(t *testing.T) {
	s := newStack(t)
	addr := "127.0.0.1:" + strconv.Itoa(s.srv.Port())

	dialAdv := func(name string, cands []string) (*ctrl.Session, *proto.Assignment) {
		sess, a, err := ctrl.Dial(addr, name, nil, 5*time.Second)
		if err != nil {
			t.Fatalf("dial %s: %v", name, err)
		}
		// Candidate 字段仅旧端使用；服务器以 Candidates 自行计算主候选
		body, _ := json.Marshal(proto.P2PAdvertise{
			Version:    proto.ProtocolVersion,
			MAC:        a.MAC,
			Candidate:  cands[0],
			Candidates: cands,
		})
		if err := sess.Send(proto.TypeP2PAdvertise, body); err != nil {
			t.Fatalf("advertise %s: %v", name, err)
		}
		return sess, a
	}

	// B 先注册（此时无其它候选 peer，无推送）
	bSess, aB := dialAdv("B", []string{"1.2.3.4:888", "192.168.1.6:888"})
	defer bSess.Close()
	// C 异出口注册 -> 推送给 B（应过滤掉 LAN）
	cSess, aC := dialAdv("C", []string{"9.9.9.9:999", "192.168.1.7:999"})
	defer cSess.Close()
	// A 与 B 同出口注册 -> 推送给 B（全候选）与 C（仅公网）
	aSess, aA := dialAdv("A", []string{"1.2.3.4:777", "192.168.1.5:777"})
	defer aSess.Close()

	// B 应收到两条推送：来自 C（公网 1 条）与来自 A（全 2 条）
	gotB := collectCandidates(t, bSess, 2, 3*time.Second)
	if c := gotB[aC.MAC]; c == nil || len(c.Candidates) != 1 || c.Candidates[0] != "9.9.9.9:999" {
		t.Fatalf("异出口 C 推给 B 应只有公网候选，实际 %+v", c)
	}
	if c := gotB[aA.MAC]; c == nil || len(c.Candidates) != 2 {
		t.Fatalf("同出口 A 推给 B 应含 LAN 候选，实际 %+v", c)
	} else {
		if c.Candidates[0] != "1.2.3.4:777" || c.Candidates[1] != "192.168.1.5:777" {
			t.Fatalf("A 候选内容不符: %v", c.Candidates)
		}
		if c.Candidate != "1.2.3.4:777" {
			t.Fatalf("兼容字段 Candidate 应为过滤后首条: %q", c.Candidate)
		}
	}

	// C（异出口）应收到两条：A 的公网候选（registerPeer 推送，B 的
	// registerPeer 时 C 还未上线故由 distributeCandidates 补发 B 的公网候选）。
	// 两条都不得携带任何一方的 LAN 地址。
	gotC := collectCandidates(t, cSess, 2, 3*time.Second)
	if c := gotC[aA.MAC]; c == nil || len(c.Candidates) != 1 || c.Candidates[0] != "1.2.3.4:777" {
		t.Fatalf("异出口 C 收 A 应只有公网候选，实际 %+v", c)
	}
	if c := gotC[aB.MAC]; c != nil && (len(c.Candidates) != 1 || c.Candidates[0] != "1.2.3.4:888") {
		t.Fatalf("异出口 C 收 B 应只有公网候选，实际 %+v", c)
	}
	if len(gotC) != 2 {
		t.Fatalf("异出口 C 应只收到 2 台对端推送，实际 %d 条", len(gotC))
	}
}

// collectCandidates 从 session 收集 PeerCandidate，按 PeerMAC 索引，
// 收够 want 条或超时止。want=0 时仅按超时收集（用于"不应收到"断言）。
func collectCandidates(t *testing.T, sess *ctrl.Session, want int, timeout time.Duration) map[string]*proto.PeerCandidate {
	t.Helper()
	out := map[string]*proto.PeerCandidate{}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) && (want == 0 || len(out) < want) {
		msg, err := sess.RecvDeadline(time.Now().Add(500 * time.Millisecond))
		if err != nil {
			continue // 超时重试，直到总 deadline
		}
		if msg.Type != proto.TypePeerCandidate {
			continue
		}
		var c proto.PeerCandidate
		if json.Unmarshal(msg.Body, &c) != nil || c.PeerMAC == "" {
			continue
		}
		if _, dup := out[c.PeerMAC]; dup {
			continue
		}
		cc := c
		out[c.PeerMAC] = &cc
	}
	return out
}
