package main

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

// 已注册的子命令（与 run() 的 switch 一致）。usage()/help 必须覆盖它们，
// 防止命令增减导致 help 与实现脱节（回归：clients/status 曾漏列）。
var registeredSubcommands = []string{
	"server",
	"client",
	"service",
	"service-run",
	"stun-server",
	"status",
	"clients",
	"version",
	"help",
}

// TestUsageContainsVersion - usage 输出必须带版本号（版本 %s），否则无参数/help 不显示版本。
func TestUsageContainsVersion(t *testing.T) {
	out := captureStdout(t, usage)
	if !strings.Contains(out, "版本") {
		t.Fatalf("usage 缺少版本标记: %q", out)
	}
	if !bytes.Contains([]byte(out), []byte(Version)) {
		t.Fatalf("usage 未含版本值 %q: %q", Version, out)
	}
}

// TestUsageListsAllSubcommands - 每个已注册子命令都必须在 usage 中出现。
func TestUsageListsAllSubcommands(t *testing.T) {
	out := captureStdout(t, usage)
	for _, cmd := range registeredSubcommands {
		if !strings.Contains(out, cmd) {
			t.Errorf("usage 缺少子命令 %q; 输出:\n%s", cmd, out)
		}
	}
}

// TestUsageNoArgPrintsVersionAndUsage - 无参数运行必须打印带版本的 usage 并返回退出码 2。
func TestUsageNoArgPrintsVersionAndUsage(t *testing.T) {
	out := captureStdout(t, func() {
		if code := run([]string{}); code != 2 {
			t.Errorf("run([]string{}) 退出码=%d, want 2", code)
		}
	})
	if !strings.Contains(out, "版本") || !bytes.Contains([]byte(out), []byte(Version)) {
		t.Fatalf("无参数输出未含版本号: %q", out)
	}
	for _, cmd := range registeredSubcommands {
		if !strings.Contains(out, cmd) {
			t.Errorf("无参数 usage 缺少子命令 %q", cmd)
		}
	}
}

// TestUsageHelpFlagsZero - -h/--help/help 打印含版本的 usage 并返回 0。
func TestUsageHelpFlagsZero(t *testing.T) {
	for _, arg := range []string{"-h", "--help", "help"} {
		out := captureStdout(t, func() {
			if code := run([]string{arg}); code != 0 {
				t.Errorf("run([%q]) 退出码=%d, want 0", arg, code)
			}
		})
		if !strings.Contains(out, "版本") || !bytes.Contains([]byte(out), []byte(Version)) {
			t.Errorf("run([%q]) 输出未含版本号: %q", arg, out)
		}
	}
}

// TestUsageUnknownCommand - 未知子命令打印 usage 并返回 2。
func TestUsageUnknownCommand(t *testing.T) {
	if code := run([]string{"bogus-subcmd"}); code != 2 {
		t.Fatalf("run([bogus]) 退出码=%d, want 2", code)
	}
}

// TestVersionCommandPrintsVersion - version 子命令打印版本。
func TestVersionCommandPrintsVersion(t *testing.T) {
	out := captureStdout(t, func() {
		if code := run([]string{"version"}); code != 0 {
			t.Errorf("run([version]) 退出码=%d, want 0", code)
		}
	})
	if !bytes.Contains([]byte(out), []byte(Version)) {
		t.Fatalf("version 输出未含版本: %q", out)
	}
}

// TestVersionFormat - 版本号必须是 semver[+hash] 格式，且源码默认值与注入格式一致。
// 注: go test 不经过 build.sh 的 -ldflags，Version 恒为源码默认值（无 +hash）；
// 此测试锁定"格式契约"（MAJOR.MINOR.PATCH[+hash]），防止 build.sh 注入的
// main.Version 格式与 usage 输出失配（回归：曾只有 bare 1.0.0 无 +hash）。
func TestVersionFormat(t *testing.T) {
	if !semverRe.MatchString(Version) {
		t.Fatalf("Version=%q 不符合 semver[+hash] 格式", Version)
	}
}

var semverRe = regexp.MustCompile(`^\d+\.\d+\.\d+(\+[0-9a-zA-Z.-]+)?$`)
