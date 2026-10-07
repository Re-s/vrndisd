// Package tap 抽象虚拟网卡（TAP）。
//
// 三种实现：
//   - LinuxTAP   真实 Linux TAP（/dev/net/tun，需 root）—— 数据面完整支持
//   - WindowsTAP wintun 适配器（需 wintun.dll 与管理员权限）
//   - Fake       内存网卡 —— 测试用，全平台可用
package tap

import (
	"errors"
	"net"
	"sync"
)

// Device 一块虚拟网卡。隧道逻辑只依赖此接口。
type Device interface {
	// Read 读取一帧（阻塞）。
	Read() ([]byte, error)
	// Write 写出一帧。
	Write(frame []byte) error
	// SetMAC 设置网卡 MAC。
	SetMAC(mac net.HardwareAddr) error
	// SetIPv4 配置 IPv4 地址与前缀。
	SetIPv4(ip net.IP, prefix int) error
	// Up 置为启用。
	Up() error
	// AddDefaultRoute 把系统默认路由指向本网卡（可选功能）。
	AddDefaultRoute(gateway net.IP) error
	// Name 网卡名。
	Name() string
	// Close 关闭网卡（真实 TAP 会随之销毁接口）。
	Close() error
}

// ErrUnsupported 平台不支持或缺少驱动。
var ErrUnsupported = errors.New("vrndis/tap: unsupported platform or missing driver")

// Fake 内存网卡（测试用）。
//
//	隧道侧: Read/Write（Server/Client 使用）
//	测试侧: Inject/Capture（模拟操作系统收发帧）
type Fake struct {
	name      string
	toTunnel  chan []byte
	toApp     chan []byte
	closed    chan struct{}
	closeOnce sync.Once

	MAC    net.HardwareAddr
	IPv4   string
	UpFlag bool
	Route  net.IP
}

// NewFake 创建内存网卡。
func NewFake(name string) *Fake {
	return &Fake{
		name:     name,
		toTunnel: make(chan []byte, 256),
		toApp:    make(chan []byte, 256),
		closed:   make(chan struct{}),
	}
}

func (f *Fake) Read() ([]byte, error) {
	select {
	case frame := <-f.toTunnel:
		return frame, nil
	case <-f.closed:
		return nil, errors.New("vrndis/tap: device closed")
	}
}

func (f *Fake) Write(frame []byte) error {
	buf := append([]byte(nil), frame...)
	select {
	case f.toApp <- buf:
		return nil
	case <-f.closed:
		return errors.New("vrndis/tap: device closed")
	}
}

func (f *Fake) SetMAC(mac net.HardwareAddr) error { f.MAC = mac; return nil }
func (f *Fake) SetIPv4(ip net.IP, prefix int) error {
	f.IPv4 = ip.String() + "/" + itoa(prefix)
	return nil
}
func (f *Fake) Up() error                            { f.UpFlag = true; return nil }
func (f *Fake) AddDefaultRoute(gateway net.IP) error { f.Route = gateway; return nil }
func (f *Fake) Name() string                         { return f.name }

func (f *Fake) Close() error {
	f.closeOnce.Do(func() { close(f.closed) })
	return nil
}

// Inject 模拟操作系统向网卡注入一帧（测试侧）。
func (f *Fake) Inject(frame []byte) error {
	buf := append([]byte(nil), frame...)
	select {
	case f.toTunnel <- buf:
		return nil
	case <-f.closed:
		return errors.New("vrndis/tap: device closed")
	}
}

// Capture 模拟操作系统从网卡收取一帧（测试侧，阻塞）。
func (f *Fake) Capture() ([]byte, error) {
	select {
	case frame := <-f.toApp:
		return frame, nil
	case <-f.closed:
		return nil, errors.New("vrndis/tap: device closed")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [4]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
