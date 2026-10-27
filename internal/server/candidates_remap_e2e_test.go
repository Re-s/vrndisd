package server_test

// 回归：对端下线/地址漂移后，服务器必须重发新候选给"自以为仍与它直连"的 peer。
//
// 事故链（缺陷 B 服务器侧，2026-10-26 HK 重启现场）：
//  1. A 与 B 打洞成功，A 上报 P2PReady->B，服务器记录 session.readyPeer=B
//  2. B 重启（或 NAT 重映射），旧地址作废
//  3. B 以新地址重新注册候选
//  4. pushCandidateToOthers 见 A "已与 B 直连"而跳过推送
//  5. A 的路由永远指向 B 的旧地址 -> UDP 静默丢包
//
// 修复：unregisterPeer/registerPeer(主候选变化) 时清除指向该 MAC 的
// p2p-ready 标记，使新候选重新下发给所有相关会话。

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/Re-s/vrndisd/internal/ctrl"
	"github.com/Re-s/vrndisd/internal/proto"
)

func TestPeerRemapRepushesCandidateToReadyPeer(t *testing.T) {
	s := newStack(t)
	addr := "127.0.0.1:" + strconv.Itoa(s.srv.Port())

	dialAdv := func(name string, cands []string) (*ctrl.Session, *proto.Assignment) {
		sess, a, err := ctrl.Dial(addr, name, nil, 5*time.Second)
		if err != nil {
			t.Fatalf("dial %s: %v", name, err)
		}
		body, _ := json.Marshal(proto.P2PAdvertise{
			Version:    proto.ProtocolVersion,
			MAC:        a.MAC,
			Candidates: cands,
		})
		if err := sess.Send(proto.TypeP2PAdvertise, body); err != nil {
			t.Fatalf("advertise %s: %v", name, err)
		}
		return sess, a
	}

	// A、B 上线并互通候选
	aSess, aA := dialAdv("A", []string{"1.2.3.4:1000"})
	defer aSess.Close()
	bSess, aB := dialAdv("B", []string{"5.6.7.8:2000"})
	// A 收到 B 的候选后上报 P2PReady -> B（模拟打洞成功）
	gotA := collectCandidates(t, aSess, 1, 3*time.Second)
	if c := gotA[aB.MAC]; c == nil || c.Candidates[0] != "5.6.7.8:2000" {
		t.Fatalf("A 未收到 B 的初始候选: %+v", gotA)
	}
	ready, _ := json.Marshal(proto.P2PReady{Version: proto.ProtocolVersion, PeerMAC: aB.MAC})
	if err := aSess.Send(proto.TypeP2PReady, ready); err != nil {
		t.Fatalf("A 上报 P2PReady: %v", err)
	}
	// 同样让 B 上报对 A 的 ready（双向标记）
	gotB := collectCandidates(t, bSess, 1, 3*time.Second)
	if c := gotB[aA.MAC]; c == nil {
		t.Fatalf("B 未收到 A 的初始候选: %+v", gotB)
	}
	readyA, _ := json.Marshal(proto.P2PReady{Version: proto.ProtocolVersion, PeerMAC: aA.MAC})
	if err := bSess.Send(proto.TypeP2PReady, readyA); err != nil {
		t.Fatalf("B 上报 P2PReady: %v", err)
	}
	// 排空双方后续消息，避免误判
	collectCandidates(t, aSess, 0, 300*time.Millisecond)

	// B 重启：同名（粘滞租约）拿到相同 MAC，以新地址重新注册
	bSess.Close()
	b2Sess, aB2 := dialAdv("B", []string{"5.6.7.8:3000"})
	defer b2Sess.Close()
	if aB2.MAC != aB.MAC {
		t.Fatalf("粘滞租约应复用 MAC: got %s want %s", aB2.MAC, aB.MAC)
	}

	// 红线：A 必须收到 B 的新候选（旧实现因 A 的 readyPeer=B 而跳过）
	gotA2 := collectCandidates(t, aSess, 1, 3*time.Second)
	c, ok := gotA2[aB2.MAC]
	if !ok {
		t.Fatalf("B 重映射后 A 未收到新候选（仍被困在死路由）: %+v", gotA2)
	}
	if c.Candidates[0] != "5.6.7.8:3000" {
		t.Fatalf("A 收到的应是 B 的新地址 %s: %+v", "5.6.7.8:3000", c)
	}

	// 同一 MAC 不断线但主候选漂移（NAT 重映射）时也必须重推
	body, _ := json.Marshal(proto.P2PAdvertise{
		Version:    proto.ProtocolVersion,
		MAC:        aB2.MAC,
		Candidates: []string{"5.6.7.8:4000"},
	})
	if err := b2Sess.Send(proto.TypeP2PAdvertise, body); err != nil {
		t.Fatalf("B 二次注册候选: %v", err)
	}
	// A 先重新上报 ready（模拟 B 重启后重新打洞成功）
	if err := aSess.Send(proto.TypeP2PReady, ready); err != nil {
		t.Fatalf("A 再次上报 P2PReady: %v", err)
	}
	gotA3 := collectCandidates(t, aSess, 1, 3*time.Second)
	if c, ok := gotA3[aB2.MAC]; !ok || c.Candidates[0] != "5.6.7.8:4000" {
		t.Fatalf("B 主候选漂移后 A 未收到新候选: %+v", gotA3)
	}
}
