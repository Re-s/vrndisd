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
	// SetMTU 设置网卡 MTU（mtu<=0 表示不设置，保持系统默认）。
	SetMTU(mtu int) error
	// Up 置为启用。
	Up() error
	// AddDefaultRoute 把系统默认路由指向本网卡（可选功能）。
	AddDefaultRoute(gateway net.IP) error
	// Name 网卡名。
	Name() string
	// SetPeerInfo 设置本端与对端网关的二层/三层寻址信息，
	// 供 L2<->L3 适配层使用。仅 Windows wintun(三层 TUN)需要该信息
	// 在 IP 包与以太网帧之间转换；Linux/Android TAP 是真实二层设备，
	// 该方法为空实现。
	SetPeerInfo(localMAC, gatewayMAC net.HardwareAddr, localIP net.IP) error
	// Close 关闭网卡（真实 TAP 会随之销毁接口）。
	Close() error
}

// ErrUnsupported 平台不支持或缺少驱动。
var ErrUnsupported = errors.New("vrndis/tap: unsupported platform or missing driver")

// ErrBufferFull Fake 网卡接收队列已满。
var ErrBufferFull = errors.New("vrndis/tap: device buffer full")

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

	// SetPeerInfo 注入的对端信息（测试断言用）。
	PeerMAC    net.HardwareAddr
	GatewayMAC net.HardwareAddr
	PeerIP     net.IP
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
	if f.isClosed() {
		return errors.New("vrndis/tap: device closed")
	}
	buf := append([]byte(nil), frame...)
	select {
	case f.toApp <- buf:
		return nil
	case <-f.closed:
		return errors.New("vrndis/tap: device closed")
	default:
		return ErrBufferFull
	}
}

func (f *Fake) SetMAC(mac net.HardwareAddr) error { f.MAC = mac; return nil }
func (f *Fake) SetIPv4(ip net.IP, prefix int) error {
	f.IPv4 = ip.String() + "/" + itoa(prefix)
	return nil
}
func (f *Fake) Up() error                            { f.UpFlag = true; return nil }
func (f *Fake) SetMTU(mtu int) error                 { return nil }
func (f *Fake) AddDefaultRoute(gateway net.IP) error { f.Route = gateway; return nil }
func (f *Fake) Name() string                         { return f.name }

// SetPeerInfo Fake 网卡本身就是二层，无需 L2↔L3 转换；
// 仅记录注入的对端信息供测试断言。
func (f *Fake) SetPeerInfo(localMAC, gatewayMAC net.HardwareAddr, localIP net.IP) error {
	f.PeerMAC = append([]byte(nil), localMAC...)
	f.GatewayMAC = append([]byte(nil), gatewayMAC...)
	f.PeerIP = append([]byte(nil), localIP...)
	return nil
}
func (f *Fake) Close() error {
	f.closeOnce.Do(func() { close(f.closed) })
	return nil
}

// Inject 模拟操作系统向网卡注入一帧（测试侧）。
func (f *Fake) Inject(frame []byte) error {
	if f.isClosed() {
		return errors.New("vrndis/tap: device closed")
	}
	// Inject 模拟操作系统向网卡注入帧，背压时阻塞（与真实 TAP 语义一致）；
	// 测试侧注入方会在队列被隧道消费后继续注入。
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

func (f *Fake) isClosed() bool {
	select {
	case <-f.closed:
		return true
	default:
		return false
	}
}
