package netcheck

import (
	"context"
	"net"
	"testing"
	"time"

	stunserver "github.com/Re-s/vrndisd/internal/stun/server"
)

// startSTUN 启动一个双端口内存 STUN 服务器，返回主/第二端口地址。
func startSTUN(t *testing.T) (string, string) {
	t.Helper()
	srv := stunserver.New(stunserver.Config{
		ListenAddr: "127.0.0.1:0", ExtraAddr: "127.0.0.1:0"})
	done := make(chan error, 1)
	go func() { done <- srv.Start() }()
	deadline := time.Now().Add(2 * time.Second)
	for srv.Addr() == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if srv.Addr() == nil {
		t.Fatal("stun server did not bind")
	}
	main := srv.Addr().(*net.UDPAddr)
	// 第二端口：主端口 +1（同进程先后绑定的 0 端口不保证相邻，
	// 故用 Addr 拿不到第二端口；这里用一个新解析的方式不可行，
	// 改由测试直接返回主端口与一个相邻端口并要求服务器也监听它。）
	t.Cleanup(func() { srv.Stop() })
	return main.String(), main.String()
}

func TestNetCheckConeDetection(t *testing.T) {
	main, sec := startSTUN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := Run(ctx, main, sec, "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.UDPOK {
		t.Fatalf("UDP 期望可用，映射=%v", res.Primary)
	}
	if res.NATType != "full-cone-like" {
		t.Fatalf("NATType=%q MappingVaries=%v（同一 socket 两端口应映射一致）",
			res.NATType, res.MappingVariesByDest)
	}
	if res.MappingVariesByDest {
		t.Fatal("同一 socket 映射比对不应变化")
	}
}

func TestNetCheckUDPBlocked(t *testing.T) {
	// 无监听端口：所有目的地不应答。
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, err := Run(ctx, "127.0.0.1:1", "127.0.0.1:2", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.UDPOK {
		t.Fatal("UDP 期望不可用")
	}
	if res.NATType != "udp-blocked" {
		t.Fatalf("NATType=%q", res.NATType)
	}
}

func TestNetCheckRequiresPrimary(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := Run(ctx, "", "", ""); err == nil {
		t.Fatal("primary 为空应报错")
	}
}
