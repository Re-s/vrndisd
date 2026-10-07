//go:build linux

package service

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
)

const unitPath = "/etc/systemd/system/vrndisd.service"

const unitTemplate = `[Unit]
Description=vrndis virtual RNDIS tunnel (%s)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s service-run --config %s
Restart=always
RestartSec=3
LimitNOFILE=65535
KillMode=mixed
TimeoutStopSec=10

[Install]
WantedBy=multi-user.target
`

type linuxManager struct{}

func newPlatformManager() Manager { return &linuxManager{} }

func (m *linuxManager) Install(cfgPath string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("定位可执行文件失败: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	absCfg, err := filepath.Abs(cfgPath)
	if err != nil {
		return err
	}
	unit := fmt.Sprintf(unitTemplate, exe, exe, absCfg)
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", unitPath, err)
	}
	if err := systemctl("daemon-reload"); err != nil {
		return err
	}
	if err := systemctl("enable", ServiceName); err != nil {
		return err
	}
	return systemctl("start", ServiceName)
}

func (m *linuxManager) Uninstall() error {
	_ = systemctl("stop", ServiceName)    // 未运行时忽略错误
	_ = systemctl("disable", ServiceName) // 未启用时忽略错误
	if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return systemctl("daemon-reload")
}

func (m *linuxManager) Start() error { return systemctl("start", ServiceName) }

func (m *linuxManager) Stop() error { return systemctl("stop", ServiceName) }

func (m *linuxManager) Status() (Status, error) {
	var st Status
	active, _ := exec.Command("systemctl", "is-active", ServiceName).Output()
	enabled, _ := exec.Command("systemctl", "is-enabled", ServiceName).Output()
	st.Running = trim(active) == "active"
	st.Installed = st.Running || trim(enabled) != "" && trim(enabled) != "disabled"
	if _, err := os.Stat(unitPath); err == nil {
		st.Installed = true
	}
	st.Detail = fmt.Sprintf("is-active=%s is-enabled=%s", trim(active), trim(enabled))
	return st, nil
}

// RunAsService 以服务身份运行（systemd 直接拉起进程；SIGTERM 优雅退出）。
func RunAsService(name string, fn func(ctx context.Context) error) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return fn(ctx)
}

func systemctl(args ...string) error {
	out, err := exec.Command("systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %v 失败: %v (%s)", args, err, trim(out))
	}
	return nil
}

func trim(b []byte) string {
	s := string(b)
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	return s
}
