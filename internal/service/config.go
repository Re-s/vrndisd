// Package service 提供持久化服务配置工具：
// 配置文件管理 + Linux systemd / Windows SCM 服务的安装、卸载、启停、状态查询。
package service

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ServiceName 服务名（systemd 单元 / Windows 服务名）。
const ServiceName = "vrndisd"

// Config 持久化运行配置（JSON 文件，服务启动时读取）。
type Config struct {
	Mode         string   `json:"mode"` // "server" 或 "client"
	Listen       string   `json:"listen,omitempty"`
	Network      string   `json:"network,omitempty"`
	Gateway      string   `json:"gateway,omitempty"`
	DNS          []string `json:"dns,omitempty"`
	Tap          string   `json:"tap,omitempty"`
	TapMAC       string   `json:"tap_mac,omitempty"`
	MTU          int      `json:"mtu,omitempty"`
	Server       string   `json:"server,omitempty"` // client 模式：服务器地址
	Name         string   `json:"name,omitempty"`
	DefaultRoute bool     `json:"default_route,omitempty"`
}

// DefaultConfig 按模式给出默认配置。
func DefaultConfig(mode string) Config {
	c := Config{Mode: mode}
	switch mode {
	case "server":
		c.Listen = "0.0.0.0:7777"
		c.Network = "10.77.0.0/24"
		c.Gateway = "10.77.0.1"
		c.DNS = []string{"8.8.8.8", "1.1.1.1"}
		c.Tap = "vrndis0"
		c.TapMAC = "02:00:00:00:00:00"
		c.MTU = 1500
	case "client":
		c.Tap = "vrndis-c0"
	}
	return c
}

// Validate 校验配置。
func (c *Config) Validate() error {
	switch c.Mode {
	case "server":
		if c.Listen == "" || c.Network == "" || c.Gateway == "" {
			return fmt.Errorf("vrndis service: server 模式需要 listen/network/gateway")
		}
	case "client":
		if c.Server == "" {
			return fmt.Errorf("vrndis service: client 模式需要 server 地址")
		}
	default:
		return fmt.Errorf("vrndis service: mode 必须是 server 或 client，实际 %q", c.Mode)
	}
	return nil
}

// Save 保存配置文件（含目录创建）。
func (c *Config) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

// Load 读取配置文件。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("解析配置 %s 失败: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Status 服务状态。
type Status struct {
	Installed bool
	Running   bool
	Detail    string
}

// absExecutable 定位当前可执行文件（解析符号链接后的绝对路径）。
func absExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe, nil
}

// Manager 平台相关的服务管理器。
type Manager interface {
	Install(cfgPath string) error
	Uninstall() error
	Start() error
	Stop() error
	Status() (Status, error)
}

// NewManager 返回当前平台的服务管理器。
func NewManager() Manager { return newPlatformManager() }
