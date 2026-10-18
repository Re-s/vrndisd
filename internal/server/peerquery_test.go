// 回归测试：answerPeerQuery 按 sessions 全量应答（含未上报候选的在线设备）。
// 根因修复前的行为：只回发 peers 表中"此刻持有候选"的条目，而候选在打洞
// 失败后被撤回退避，诊断瞬间几乎必落空——peers 命令永远空而 clients 有设备。
package server_test

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"vrndis/internal/ctrl"
	"vrndis/internal/proto"
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
