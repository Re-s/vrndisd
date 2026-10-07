//go:build windows

package tap

// Windows 数据面基于 WireGuard 的 wintun 驱动（wintun.dll，随二进制分发即可）。
// 网卡创建/收发包通过 wintun.dll 导出的 C API（syscall 直调，无 cgo）；
// IP 配置通过 netsh 完成。
//
// 注意：wintun 适配器 MAC 由驱动随机生成，SetMAC 为空操作（见 docs/windows.md）。

import (
	"fmt"
	"net"
	"os/exec"
	"strings"
	"syscall"
	"unsafe"
)

var (
	wintunDLL               = syscall.NewLazyDLL("wintun.dll")
	procWintunCreateAdapter = wintunDLL.NewProc("WintunCreateAdapter")
	procWintunOpenAdapter   = wintunDLL.NewProc("WintunOpenAdapter")
	procWintunCloseAdapter  = wintunDLL.NewProc("WintunCloseAdapter")
	procWintunStartSession  = wintunDLL.NewProc("WintunStartSession")
	procWintunEndSession    = wintunDLL.NewProc("WintunEndSession")
	procWintunGetReadWait   = wintunDLL.NewProc("WintunGetReadWaitEvent")
	procWintunReceivePacket = wintunDLL.NewProc("WintunReceivePacket")
	procWintunReleasePacket = wintunDLL.NewProc("WintunReleaseReceivePacket")
	procWintunAllocSend     = wintunDLL.NewProc("WintunAllocateSendPacket")
	procWintunSendPacket    = wintunDLL.NewProc("WintunSendPacket")
)

const (
	wintunRingCapacity = 1 << 22 // 4 MiB，必须为 2 的幂且 ∈ [0x40000, 0x4000000]
	errorNoMoreItems   = syscall.Errno(259)
	tunnelType         = "vrndis"
)

type windowsTAP struct {
	name     string
	adapter  uintptr
	session  uintptr
	readWait syscall.Handle
}

// Open 打开（或创建）wintun 适配器；需要管理员权限与 wintun.dll。
func Open(name string) (Device, error) {
	if err := wintunDLL.Load(); err != nil {
		return nil, fmt.Errorf("%w: 加载 wintun.dll 失败（请把 wintun.dll 放到可执行文件同目录）: %v", ErrUnsupported, err)
	}
	namep, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	typep, _ := syscall.UTF16PtrFromString(tunnelType)

	adapter, _, _ := procWintunOpenAdapter.Call(uintptr(unsafe.Pointer(namep)))
	if adapter == 0 {
		adapter, _, _ = procWintunCreateAdapter.Call(
			uintptr(unsafe.Pointer(namep)),
			uintptr(unsafe.Pointer(typep)),
			0, // 随机 GUID
		)
	}
	if adapter == 0 {
		return nil, fmt.Errorf("wintun: 创建/打开适配器 %q 失败（需要管理员权限）", name)
	}
	session, _, err := procWintunStartSession.Call(adapter, wintunRingCapacity)
	if session == 0 {
		procWintunCloseAdapter.Call(adapter)
		return nil, fmt.Errorf("wintun: 启动会话失败: %v", err)
	}
	wait, _, _ := procWintunGetReadWait.Call(session)
	return &windowsTAP{
		name:     name,
		adapter:  adapter,
		session:  session,
		readWait: syscall.Handle(wait),
	}, nil
}

func (t *windowsTAP) Name() string { return t.name }

func (t *windowsTAP) Read() ([]byte, error) {
	for {
		var size uint32
		ptr, _, err := procWintunReceivePacket.Call(t.session, uintptr(unsafe.Pointer(&size)))
		if ptr == 0 {
			if err == errorNoMoreItems {
				syscall.WaitForSingleObject(t.readWait, syscall.INFINITE)
				continue
			}
			return nil, fmt.Errorf("wintun: receive: %v", err)
		}
		frame := make([]byte, size)
		copy(frame, unsafe.Slice((*byte)(unsafe.Pointer(ptr)), int(size)))
		procWintunReleasePacket.Call(t.session, ptr)
		return frame, nil
	}
}

func (t *windowsTAP) Write(frame []byte) error {
	ptr, _, err := procWintunAllocSend.Call(t.session, uintptr(len(frame)))
	if ptr == 0 {
		return fmt.Errorf("wintun: allocate send packet: %v", err)
	}
	copy(unsafe.Slice((*byte)(unsafe.Pointer(ptr)), len(frame)), frame)
	procWintunSendPacket.Call(t.session, ptr)
	return nil
}

// SetMAC wintun 适配器 MAC 由驱动管理，此为空操作（记录语义在上层）。
func (t *windowsTAP) SetMAC(mac net.HardwareAddr) error { return nil }

func (t *windowsTAP) SetIPv4(ip net.IP, prefix int) error {
	mask := net.IP(net.CIDRMask(prefix, 32)).String()
	out, err := exec.Command("netsh", "interface", "ipv4", "set", "address",
		fmt.Sprintf("name=%s", t.name), "source=static",
		fmt.Sprintf("addr=%s", ip.String()), fmt.Sprintf("mask=%s", mask)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("netsh set address: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (t *windowsTAP) Up() error {
	out, err := exec.Command("netsh", "interface", "set", "interface",
		fmt.Sprintf("name=%s", t.name), "admin=enabled").CombinedOutput()
	if err != nil {
		return fmt.Errorf("netsh enable interface: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (t *windowsTAP) AddDefaultRoute(gateway net.IP) error {
	out, err := exec.Command("netsh", "interface", "ipv4", "add", "route",
		"0.0.0.0/0", t.name, gateway.String()).CombinedOutput()
	if err != nil {
		return fmt.Errorf("netsh add route: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (t *windowsTAP) Close() error {
	if t.session != 0 {
		procWintunEndSession.Call(t.session)
	}
	if t.adapter != 0 {
		procWintunCloseAdapter.Call(t.adapter)
	}
	return nil
}
