// 回归测试：answerPeerQuery 按 sessions 全量应答（含未上报候选的在线设备）。
// 根因修复前的行为：只回发 peers 表中"此刻持有候选"的条目，而候选在打洞
// 失败后被撤回退避，诊断瞬间几乎必落空——peers 命令永远空而 clients 有设备。
package server_test

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/Re-s/vrndisd/internal/ctrl"
	"github.com/Re-s/vrndisd/internal/proto"
)

// TestPeerQueryAnswersAllOnlineDevices 两个普通客户端（未开 P2P、无候选），
// 第三个会话发起全量 PeerQuery，应收到两条 PeerCandidate（PeerMAC 非空、
// Candidate 为空=relay-only），随后收到全空结束标记。
func TestPeerQueryAnswersAllOnlineDevices(t *testing.T) {
	s := newStack(t)
	s.connect("alice")
	s.connect("bob")

	sess, _, err := ctrl.Dial("127.0.0.1:"+strconv.Itoa(s.srv.Port()), "querier", nil, 5*time.Second)
	if err != nil {
		t.Fatalf("ctrl.Dial: %v", err)
	}
	defer sess.Close()

	body, _ := json.Marshal(proto.PeerQuery{Version: proto.ProtocolVersion})
	if err := sess.Send(proto.TypePeerQuery, body); err != nil {
		t.Fatalf("send PeerQuery: %v", err)
	}

	var got []proto.PeerCandidate
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		msg, err := sess.RecvDeadline(time.Now().Add(500 * time.Millisecond))
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		if msg.Type != proto.TypePeerCandidate {
			continue
		}
		var c proto.PeerCandidate
		if err := json.Unmarshal(msg.Body, &c); err != nil {
			continue
		}
		if c.Candidate == "" && c.PeerMAC == "" {
			break // 结束标记
		}
		got = append(got, c)
	}
	if len(got) != 2 {
		t.Fatalf("期望 2 台在线设备（alice/bob），实际收到 %d 条", len(got))
	}
	for _, c := range got {
		if c.PeerMAC == "" || c.PeerIP == "" {
			t.Errorf("relay-only 条目缺少标识: %+v", c)
		}
		if c.Candidate != "" {
			t.Errorf("未开 P2P 却收到候选: %+v", c)
		}
	}
}

// TestPeerQueryFromClientWithoutCandidates 复现 2026-10-20 生产卡死事故的
// 精确触发条件并锁定修复：
//  1. 诊断客户端（vrndisd peers）ctrl.Dial 后直接发 PeerQuery，
//     从不做 P2PAdvertise → 服务器侧 receiver 为 nil；
//  2. 另一在线 peer 持"公网 + LAN"混合候选（egressIP 非空 + 含私网候选），
//     恰好命中旧代码 candidatesFor 的 receiver.egressIP 空指针解引用。
//
// 期望：不 panic；LAN 候选不外泄（只回公网候选）；answerPeerQuery 持有的
// s.mu/peersMu 读锁不泄漏（Stop 内部取 s.mu.Lock()，若读锁被 panic
// 带走则 Stop 永远无法完成——这正是事故时 SIGTERM 无响应的机制）。
func TestPeerQueryFromClientWithoutCandidates(t *testing.T) {
	s := newStack(t)

	// bob：常驻会话，上报混合候选（公网主候选 + LAN 候选）
	bob, _, err := ctrl.Dial("127.0.0.1:"+strconv.Itoa(s.srv.Port()), "bob", nil, 5*time.Second)
	if err != nil {
		t.Fatalf("ctrl.Dial bob: %v", err)
	}
	defer bob.Close()
	advBody, _ := json.Marshal(proto.P2PAdvertise{
		Version:    proto.ProtocolVersion,
		MAC:        "02:00:00:00:00:02",
		Candidate:  "1.2.3.4:100",
		Candidates: []string{"1.2.3.4:100", "192.168.1.5:200"},
	})
	if err := bob.Send(proto.TypeP2PAdvertise, advBody); err != nil {
		t.Fatalf("bob advertise: %v", err)
	}

	// 查询者：诊断客户端，只发 PeerQuery，不上报自己的候选
	querier, _, err := ctrl.Dial("127.0.0.1:"+strconv.Itoa(s.srv.Port()), "peers", nil, 5*time.Second)
	if err != nil {
		t.Fatalf("ctrl.Dial querier: %v", err)
	}
	defer querier.Close()
	body, _ := json.Marshal(proto.PeerQuery{Version: proto.ProtocolVersion})
	if err := querier.Send(proto.TypePeerQuery, body); err != nil {
		t.Fatalf("send PeerQuery: %v", err)
	}

	// 收集应答：期望一条 bob 的条目（只含公网候选）+ 全空结束标记
	var got []proto.PeerCandidate
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		msg, err := querier.RecvDeadline(time.Now().Add(500 * time.Millisecond))
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		if msg.Type != proto.TypePeerCandidate {
			continue
		}
		var c proto.PeerCandidate
		if err := json.Unmarshal(msg.Body, &c); err != nil {
			continue
		}
		if c.Candidate == "" && c.PeerMAC == "" {
			break // 结束标记
		}
		got = append(got, c)
	}
	if len(got) != 1 {
		t.Fatalf("期望 1 条应答（bob），实际 %d 条: %+v", len(got), got)
	}
	// LAN 候选绝不下发给未知出口的接收方
	if len(got[0].Candidates) != 1 || got[0].Candidates[0] != "1.2.3.4:100" {
		t.Fatalf("nil 接收方应只收公网候选，实际: %+v", got[0].Candidates)
	}

	// 锁不泄漏断言：Stop 取 s.mu.Lock()，读锁若被 panic 永久占用，
	// 这里必然超时——即事故中"SIGTERM 无响应、只能 SIGKILL"的表现。
	done := make(chan struct{})
	go func() {
		s.srv.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("answerPeerQuery 泄漏会话表读锁：Stop 超时（读锁未释放）")
	}
}
