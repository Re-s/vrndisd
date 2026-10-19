package main

import (
	"errors"
	"strings"
	"testing"
)

// client 子命令与 netcheck/ping/peers 一致：未指定 --server 时回退
// 已安装配置；无配置时应给出可操作的报错（而非裸拨号失败）。
// 此处只覆盖"无配置"分支（不产生真实网络拨号），回退解析本身由
// resolve_peer_test.go 覆盖。

func TestCmdClientNoServerNoConfig(t *testing.T) {
	withInstalledConfig(t, nil, errors.New("no such file"))
	err := cmdClient([]string{})
	if err == nil {
		t.Fatal("无 --server 且无已安装配置应报错")
	}
	if !strings.Contains(err.Error(), "--server 必填") {
		t.Fatalf("报错应保留 --server 必填提示: got %v", err)
	}
	if !strings.Contains(err.Error(), "service install") {
		t.Fatalf("报错应提示用 service install 预置配置: got %v", err)
	}
}
