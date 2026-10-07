//go:build windows

package service

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

type windowsManager struct{}

func newPlatformManager() Manager { return &windowsManager{} }

func (m *windowsManager) Install(cfgPath string) error {
	exe, err := absExecutable()
	if err != nil {
		return err
	}
	binPath := fmt.Sprintf(`"%s" service-run --config "%s"`, exe, cfgPath)
	if err := sc("create", ServiceName,
		"binPath=", binPath,
		"start=", "auto",
		"DisplayName=", "vrndis Virtual RNDIS Tunnel"); err != nil {
		return err
	}
	_ = sc("description", ServiceName,
		"vrndis virtual RNDIS tunnel service (TCP <-> virtual NIC)")
	// 崩溃自动拉起：5s / 10s / 30s 三次重启，之后一天内累计计数清零
	_ = sc("failure", ServiceName, "reset=", "86400",
		"actions=", "restart/5000/restart/10000/restart/30000")
	return sc("start", ServiceName)
}

func (m *windowsManager) Uninstall() error {
	_ = sc("stop", ServiceName)
	return sc("delete", ServiceName)
}

func (m *windowsManager) Start() error { return sc("start", ServiceName) }

func (m *windowsManager) Stop() error { return sc("stop", ServiceName) }

func (m *windowsManager) Status() (Status, error) {
	out, err := scOutput("query", ServiceName)
	if err != nil {
		// 1060 = 服务不存在
		return Status{Installed: false}, nil
	}
	st := Status{Installed: true}
	lower := strings.ToLower(out)
	st.Running = strings.Contains(lower, "running")
	switch {
	case strings.Contains(lower, "running"):
		st.Detail = "RUNNING"
	case strings.Contains(lower, "stopped"):
		st.Detail = "STOPPED"
	default:
		st.Detail = strings.TrimSpace(out)
	}
	return st, nil
}

// RunAsService 以 Windows 服务身份运行（SCM 调度，见 scm_windows.go）。
func RunAsService(name string, fn func(ctx context.Context) error) error {
	return runWithSCM(name, fn)
}

func sc(args ...string) error {
	_, err := scOutput(args...)
	return err
}

func scOutput(args ...string) (string, error) {
	out, err := exec.Command("sc.exe", args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("sc.exe %v: %v (%s)", args, err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}
