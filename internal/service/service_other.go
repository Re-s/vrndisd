//go:build !linux && !windows

package service

import (
	"context"
	"errors"
	"os/signal"
	"syscall"
)

// ErrNoPlatform 当前平台无服务管理后端。
var ErrNoPlatform = errors.New("vrndis/service: 此平台不支持服务管理（仅 Linux/Windows）")

type otherManager struct{}

func newPlatformManager() Manager { return &otherManager{} }

func (m *otherManager) Install(cfgPath string) error { return ErrNoPlatform }
func (m *otherManager) Uninstall() error             { return ErrNoPlatform }
func (m *otherManager) Start() error                 { return ErrNoPlatform }
func (m *otherManager) Stop() error                  { return ErrNoPlatform }
func (m *otherManager) Status() (Status, error)      { return Status{}, ErrNoPlatform }

// RunAsService 前台运行（等价于服务）。
func RunAsService(name string, fn func(ctx context.Context) error) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	return fn(ctx)
}
