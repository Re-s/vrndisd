package main

import (
	"errors"
	"strings"
	"testing"

	"vrndis/internal/service"
)

// netcheck 缺省服务器解析：不指定 --server 时应自动取"连接的服务器"
// （已安装配置 client 模式取其服务器、server 模式回环本机），
// 且 STUN 端口固定 5478/5479（与客户端 candidateAddr 同一推导，
// 不得误用控制端口 7777）。

func TestNetcheckDefaultServerFromClientConfig(t *testing.T) {
	withInstalledConfig(t, &service.Config{
		Mode:   "client",
		Server: "5.6.7.8:7777",
		PSK:    "cfg-psk",
	}, nil)
	primary, secondary, err := netcheckDefaultServer("")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if primary != "5.6.7.8:5478" {
		t.Fatalf("primary STUN 应取服务器 host + 5478: got %q", primary)
	}
	if secondary != "5.6.7.8:5479" {
		t.Fatalf("secondary STUN 应为 5479: got %q", secondary)
	}
}

func TestNetcheckDefaultServerFromServerConfig(t *testing.T) {
	withInstalledConfig(t, &service.Config{
		Mode:   "server",
		Listen: "0.0.0.0:7777",
		PSK:    "srv-psk",
	}, nil)
	primary, secondary, err := netcheckDefaultServer("")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if primary != "127.0.0.1:5478" {
		t.Fatalf("server 模式应回环本机 STUN: got %q", primary)
	}
	if secondary != "127.0.0.1:5479" {
		t.Fatalf("secondary STUN 应为 5479: got %q", secondary)
	}
}

func TestNetcheckDefaultServerNoConfig(t *testing.T) {
	withInstalledConfig(t, nil, errors.New("no such file"))
	_, _, err := netcheckDefaultServer("")
	if err == nil {
		t.Fatal("无已安装配置应报错")
	}
	if !strings.Contains(err.Error(), "--server 必填") {
		t.Fatalf("报错应保留 service install 预置提示: got %v", err)
	}
}

func TestNetcheckDefaultServerConfigWithoutPort(t *testing.T) {
	// 配置里的服务器地址缺端口（.SplitHostPort 失败）也不能崩
	withInstalledConfig(t, &service.Config{
		Mode:   "client",
		Server: "5.6.7.8",
	}, nil)
	primary, secondary, err := netcheckDefaultServer("")
	if err != nil || primary != "5.6.7.8:5478" || secondary != "5.6.7.8:5479" {
		t.Fatalf("无端口配置应回退 5478/5479: got %q %q %v", primary, secondary, err)
	}
}
