package client

import (
	"testing"

	"github.com/Re-s/vrndisd/internal/proto"
)

// TestManagementStateBasic - 未连接实例的状态快照：role=client、无分配、计数为零。
func TestManagementStateBasic(t *testing.T) {
	c := New(Config{Server: "1.2.3.4:7777", Name: "x16", TapName: "fake"})
	st := c.managementState()
	if st.Role != "client" {
		t.Fatalf("Role 期望 client，得 %q", st.Role)
	}
	if st.Client == nil {
		t.Fatal("Client 状态缺失")
	}
	if st.Client.Name != "x16" || st.Client.Server != "1.2.3.4:7777" {
		t.Fatalf("名称/服务器不符: %+v", st.Client)
	}
	if st.Client.Connected || st.Client.Connects != 0 {
		t.Fatalf("未连接实例应为未连接且计数 0: %+v", st.Client)
	}
	if st.Client.VirtualIP != "" || st.Client.P2P.Enabled {
		t.Fatalf("未分配时不应有虚拟 IP: %+v", st.Client)
	}
}

// TestManagementStateAssigned - 有分配与建连计数后字段齐全。
func TestManagementStateAssigned(t *testing.T) {
	c := New(Config{Server: "1.2.3.4:7777", Name: "x16", TapName: "fake", P2P: true})
	c.assignMu.Lock()
	c.assign = &proto.Assignment{
		IP:      "10.77.0.3",
		MAC:     "02:00:00:00:00:03",
		Gateway: "10.77.0.1",
	}
	c.assignMu.Unlock()
	c.connects.Add(2)

	st := c.managementState()
	if st.Client.VirtualIP != "10.77.0.3" || st.Client.VirtualMAC != "02:00:00:00:00:03" || st.Client.Gateway != "10.77.0.1" {
		t.Fatalf("分配字段不符: %+v", st.Client)
	}
	if st.Client.Connects != 2 {
		t.Fatalf("建连计数期望 2，得 %d", st.Client.Connects)
	}
	if !st.Client.P2P.Enabled {
		t.Fatal("P2P 应标记为已启用")
	}
	if st.Client.P2P.Active || st.Client.P2P.LastRxAgo != "" {
		t.Fatalf("无隧道时不应有活跃隧道状态: %+v", st.Client.P2P)
	}
}

// TestManagementStateLastFail - 打洞失败原因应体现在状态中。
func TestManagementStateLastFail(t *testing.T) {
	c := New(Config{Server: "1.2.3.4:7777", Name: "x16", TapName: "fake", P2P: true})
	c.p2pMu.Lock()
	c.lastFail = "等待对端候选超时"
	c.p2pMu.Unlock()

	st := c.managementState()
	if st.Client.P2P.LastFail != "等待对端候选超时" {
		t.Fatalf("失败原因不符: %q", st.Client.P2P.LastFail)
	}
}
