// Package service 提供持久化服务配置工具：
// 配置文件管理 + Linux systemd / Windows SCM 服务的安装、卸载、启停、状态查询。
package service

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// ServiceName 服务名（systemd 单元 / Windows 服务名）。
const ServiceName = "vrndisd"

// Duration 是 time.Duration 的 JSON 友好封装：序列化为 "30s" 形式的可读字符串，
// 反序列化同时接受字符串（"30s"）或裸数字（纳秒），兼容旧配置。
type Duration time.Duration

// MarshalJSON 将时长序列化为带引号的可读字符串，如 "30s"。
func (d Duration) MarshalJSON() ([]byte, error) {
	return []byte(`"` + time.Duration(d).String() + `"`), nil
}

// UnmarshalJSON 接受 "30s" 形式字符串或裸数字（纳秒）。
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		v, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("vrndis service: 无效时长 %q: %w", s, err)
		}
		*d = Duration(v)
		return nil
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("vrndis service: 无效时长 %s: %w", b, err)
	}
	*d = Duration(n)
	return nil
}

// String 供日志输出与 flag.Value 使用。
func (d Duration) String() string { return time.Duration(d).String() }

// Config 持久化运行配置（JSON 文件，服务启动时读取）。
// 该结构只承载纯数据：不含连接句柄、进程句柄等运行时状态，可完整往返序列化。
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

	// 服务器侧：P2P 与内置 STUN。
	P2P        bool   `json:"p2p,omitempty"`
	STUN       bool   `json:"stun,omitempty"`
	STUNListen string `json:"stun_listen,omitempty"`

	// 加密
	PSK string `json:"psk,omitempty"`

	// 客户端侧：P2P、STUN 与自愈参数。
	STUNServer        string   `json:"stun_server,omitempty"`
	Reconnect         bool     `json:"reconnect,omitempty"`
	MaxRetries        int      `json:"max_retries,omitempty"`
	ReconnectBackoff  Duration `json:"reconnect_backoff,omitempty"`
	HeartbeatInterval Duration `json:"heartbeat_interval,omitempty"`
	HeartbeatTimeout  Duration `json:"heartbeat_timeout,omitempty"`
}

// MarshalJSON 序列化可持久化字段。
// Config 仅含纯数据（无连接/进程句柄）；persistent 别名在将来新增运行时字段时，
// 提供一处显式控制"落盘字段集合"的扩展点。
func (c *Config) MarshalJSON() ([]byte, error) {
	type persistent Config
	return json.Marshal((*persistent)(c))
}

// DefaultConfigPath 返回当前平台的默认配置文件路径：
// Linux: /etc/vrndisd.conf；Windows: %ProgramData%\vrndisd\vrndisd.conf。
func DefaultConfigPath() string {
	if runtime.GOOS == "windows" {
		if base := os.Getenv("ProgramData"); base != "" {
			return filepath.Join(base, "vrndisd", "vrndisd.conf")
		}
		return filepath.Join(".", "vrndisd.conf")
	}
	return "/etc/vrndisd.conf"
}

// DefaultConfig 按模式给出默认配置（server/client 各自补齐全部选项）。
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
		c.STUNListen = ":5478"
	case "client":
		c.Tap = "vrndis-c0"
		c.Reconnect = true
		c.MaxRetries = 0
		c.ReconnectBackoff = Duration(time.Second)
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

// Save 保存配置文件（含目录创建，0600 权限）。
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
// Install 同时把配置写入 cfgPath 并注册系统服务，保证"已安装即有配置文件"。
type Manager interface {
	Install(cfg *Config, cfgPath string) error
	Uninstall() error
	Start() error
	Stop() error
	Status() (Status, error)
}

// NewManager 返回当前平台的服务管理器。
func NewManager() Manager { return newPlatformManager() }
