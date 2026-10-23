package main

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"vrndis/internal/service"
)

// withInstalledConfig 临时把 loadInstalledConfig 替换为返回指定配置，测试结束还原。
func withInstalledConfig(t *testing.T, cfg *service.Config, loadErr error) {
	t.Helper()
	orig := loadInstalledConfig
	loadInstalledConfig = func() (*service.Config, error) { return cfg, loadErr }
	t.Cleanup(func() { loadInstalledConfig = orig })
}

func TestResolvePeerFlagsExplicitServer(t *testing.T) {
	s, p, n, err := resolvePeerFlags("1.2.3.4:7777", "cli-psk", "cli-name", "")
	if err != nil || s != "1.2.3.4:7777" || p != "cli-psk" || n != "cli-name" {
		t.Fatalf("显式 flag 应原样透传: got %q %q %q %v", s, p, n, err)
	}
}

func TestResolvePeerFlagsNoConfig(t *testing.T) {
	withInstalledConfig(t, nil, errors.New("no such file"))
	_, _, _, err := resolvePeerFlags("", "", "", "")
	if err == nil || !strings.Contains(err.Error(), "--server 必填") {
		t.Fatalf("缺省且无配置应报 --server 必填: got %v", err)
	}
	if !strings.Contains(err.Error(), "service install") {
		t.Fatalf("错误应提示 service install 预置: got %v", err)
	}
}

func TestResolvePeerFlagsClientFallback(t *testing.T) {
	withInstalledConfig(t, &service.Config{
		Mode:   "client",
		Server: "5.6.7.8:7777",
		PSK:    "cfg-psk",
		Name:   "cfg-name",
	}, nil)
	s, p, n, err := resolvePeerFlags("", "", "", "")
	if err != nil || s != "5.6.7.8:7777" || p != "cfg-psk" || n != "cfg-name" {
		t.Fatalf("client 配置回退失败: got %q %q %q %v", s, p, n, err)
	}
}

func TestResolvePeerFlagsClientMissingServer(t *testing.T) {
	withInstalledConfig(t, &service.Config{Mode: "client", Name: "x"}, nil)
	_, _, _, err := resolvePeerFlags("", "", "", "")
	if err == nil || !strings.Contains(err.Error(), "缺少 server 地址") {
		t.Fatalf("client 无 server 应报错: got %v", err)
	}
}

func TestResolvePeerFlagsServerModeLoopback(t *testing.T) {
	withInstalledConfig(t, &service.Config{
		Mode:   "server",
		Listen: "0.0.0.0:7777",
		PSK:    "srv-psk",
	}, nil)
	s, p, n, err := resolvePeerFlags("", "", "", "")
	if err != nil || s != "127.0.0.1:7777" || p != "srv-psk" {
		t.Fatalf("server 模式应回环直连本机: got %q %q %q %v", s, p, n, err)
	}
}

func TestResolvePeerFlagsCLIPskOverridesConfig(t *testing.T) {
	withInstalledConfig(t, &service.Config{Mode: "client", Server: "5.6.7.8:7777", PSK: "cfg-psk"}, nil)
	_, p, _, err := resolvePeerFlags("", "cli-psk", "", "")
	if err != nil || p != "cli-psk" {
		t.Fatalf("命令行 --psk 应优先: got %q %v", p, err)
	}
}

func TestResolvePeerFlagsBadMode(t *testing.T) {
	withInstalledConfig(t, &service.Config{Mode: "bogus"}, nil)
	_, _, _, err := resolvePeerFlags("", "", "", "")
	if err == nil || !strings.Contains(err.Error(), "mode 无效") {
		t.Fatalf("非法 mode 应报错: got %v", err)
	}
}

func TestLoopbackAddr(t *testing.T) {
	cases := map[string]string{
		"0.0.0.0:7777":  "127.0.0.1:7777",
		"[::]:7777":     "127.0.0.1:7777",
		":7777":         "127.0.0.1:7777",
		"1.2.3.4:7777":  "1.2.3.4:7777",
		"127.0.0.1:999": "127.0.0.1:999",
		"garbage":       "garbage",
	}
	for in, want := range cases {
		if got := loopbackAddr(in); got != want {
			t.Errorf("loopbackAddr(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolvePeerFlagsCustomConfig(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/vrndisd.conf"
	cfg := service.Config{Mode: "client", Server: "9.8.7.6:7777", PSK: "file-psk", Name: "file-name"}
	data, _ := json.Marshal(&cfg)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	// 已安装配置"不存在"，自定义路径应优先生效
	withInstalledConfig(t, nil, errors.New("no such file"))
	s, p, n, err := resolvePeerFlags("", "", "", path)
	if err != nil || s != "9.8.7.6:7777" || p != "file-psk" || n != "file-name" {
		t.Fatalf("--config 回退失败: got %q %q %q %v", s, p, n, err)
	}
	// --server 与 --config 共存：flag 服务器优先，psk/name 仍从文件补齐
	s, p, n, err = resolvePeerFlags("1.1.1.1:7777", "", "", path)
	if err != nil || s != "1.1.1.1:7777" || p != "file-psk" || n != "file-name" {
		t.Fatalf("flag 服务器应覆盖配置: got %q %q %q %v", s, p, n, err)
	}
}

func TestResolvePeerFlagsCustomConfigNotFound(t *testing.T) {
	_, _, _, err := resolvePeerFlags("", "", "", "/nonexistent/vrndisd.conf")
	if err == nil || !strings.Contains(err.Error(), "--config /nonexistent/vrndisd.conf 读取失败") {
		t.Fatalf("--config 文件不存在应报明确错误: got %v", err)
	}
}
