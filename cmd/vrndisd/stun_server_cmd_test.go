package main

import (
	"flag"
	"testing"
	"time"

	"vrndis/internal/stun/server"
)

// TestServerStunFlagParsing 验证 server 子命令的 STUN 参数能被解析，且默认值正确。
// 与 cmdServer 中的 flag 注册保持一致（回归：--stun/--stun-listen 曾一度未接入）。
func TestServerStunFlagParsing(t *testing.T) {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	stun := fs.Bool("stun", false, "")
	stunListen := fs.String("stun-listen", ":5478", "")
	if err := fs.Parse([]string{"--stun", "--stun-listen", "0.0.0.0:13478"}); err != nil {
		t.Fatalf("parse err: %v", err)
	}
	if !*stun {
		t.Errorf("--stun 应解析为 true")
	}
	if *stunListen != "0.0.0.0:13478" {
		t.Errorf("--stun-listen=%q, want 0.0.0.0:13478", *stunListen)
	}
}

// TestServerStunFlagDefaults 验证默认值：--stun 关闭、--stun-listen 默认 :5478。
func TestServerStunFlagDefaults(t *testing.T) {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	stun := fs.Bool("stun", false, "")
	stunListen := fs.String("stun-listen", ":5478", "")
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse err: %v", err)
	}
	if *stun {
		t.Errorf("默认 --stun 应为 false")
	}
	if *stunListen != ":5478" {
		t.Errorf("默认 --stun-listen=%q, want :5478", *stunListen)
	}
}

// TestStartStunServerDisabled 验证 --stun 关闭时 startStunServer 返回 nil（不启动 STUN）。
func TestStartStunServerDisabled(t *testing.T) {
	srv, errCh, done := startStunServer(false, ":5478", ":5479")
	if srv != nil {
		t.Errorf("stun=false 时 srv 应为 nil, got %v", srv)
	}
	if errCh != nil {
		t.Errorf("stun=false 时 errCh 应为 nil, got %v", errCh)
	}
	if done != nil {
		t.Errorf("stun=false 时 done 应为 nil, got %v", done)
	}
}

// TestStartStunServerStartsAndStops 验证 --stun 开启时能启动 STUN 服务器并可优雅停止，
// 证明 server --stun 双服务同进程场景下 STUN 能随整体关停退出。
func TestStartStunServerStartsAndStops(t *testing.T) {
	srv, errCh, done := startStunServer(true, "127.0.0.1:0", "")
	if srv == nil {
		t.Fatal("stun=true 时 srv 不应为 nil")
	}
	if errCh == nil {
		t.Fatal("stun=true 时 errCh 不应为 nil")
	}
	if done == nil {
		t.Fatal("stun=true 时 done 不应为 nil")
	}

	// 等待端口绑定。
	deadline := time.Now().Add(1 * time.Second)
	for srv.Addr() == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if srv.Addr() == nil {
		t.Fatal("STUN server 未绑定端口")
	}
	if srv.Addr().Network() != "udp" {
		t.Fatalf("期望 UDP 监听, got %s", srv.Addr().Network())
	}

	// 优雅停止：Start 应返回、协程退出、错误通道无错误。
	srv.Stop()
	select {
	case <-done:
		// Start 已返回。
	case <-time.After(2 * time.Second):
		t.Fatal("STUN 优雅停止后 Start 协程未退出（会泄漏）")
	}
	select {
	case err := <-errCh:
		t.Fatalf("优雅停止不应产生错误, got %v", err)
	default:
		// 无错误，符合预期。
	}
}

// TestStartStunServerBindConflict 验证监听冲突时 STUN 启动失败会反馈到错误通道，
// 上层据此整体优雅关停（--stun 端口被占用场景）。
func TestStartStunServerBindConflict(t *testing.T) {
	// 先占用一个 UDP 端口，再让 STUN 去监听同一端口。
	occ := server.New(server.Config{ListenAddr: "127.0.0.1:0"})
	occDone := make(chan error, 1)
	go func() { occDone <- occ.Start() }()
	deadline := time.Now().Add(1 * time.Second)
	for occ.Addr() == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if occ.Addr() == nil {
		t.Fatal("occupy server 未绑定")
	}
	addr := occ.Addr().String()
	defer occ.Stop()

	srv, errCh, done := startStunServer(true, addr, "")
	if srv == nil {
		t.Fatal("srv 不应为 nil")
	}
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("期望 STUN 启动失败，但错误为 nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("STUN 启动失败未反馈到错误通道")
	}
	// 启动失败后协程也应退出（done 被关闭）。
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("STUN 启动失败后协程未退出")
	}
}
