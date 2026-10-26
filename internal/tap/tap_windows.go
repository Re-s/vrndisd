//go:build windows

package tap

// Windows 数据面基于 WireGuard 的 wintun 驱动（wintun.dll，随二进制分发即可）。
// 网卡创建/收发包通过 wintun.dll 导出的 C API（syscall 直调，无 cgo）；
// IP 配置通过 netsh 完成。
//
// 注意：wintun 适配器 MAC 由驱动随机生成，SetMAC 为空操作（见 docs/windows.md）。
//
// L2↔L3 适配（方案 B）：wintun 是三层 TUN 驱动，只收发裸 IPv4/IPv6 包，
// 而 vrndis 数据面是二层以太网帧。本文件在 wintun 上方调用
// internal/tap/l2l3 完成转换：
//   - Read（上行）：裸 IP 包 → 伪以太网帧（补 14 字节头，Dst 按
//     网关/ARP 缓存/广播三级决策）
//   - Write（下行）：以太网帧 → 裸 IP 包（剥 14 字节头）；ARP 帧被
//     l2l3 拦截，若请求的是本机 IP，构造的 ARP Reply 经 arpInject
//     回调直接发回服务器（不进 wintun，否则被静默丢弃），从而让
//     服务器 Linux 内核的 ARP 缓存能解析到本机 MAC。
//
// MTU：wintun 侧设为 Assignment.MTU-14（如 1486），保证 Windows 发出的
// IP 包加伪头后 ≤1500，与服务器 TAP 的 1500 MTU 对齐。

import (
	"fmt"
	"io"
	"net"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/Re-s/vrndisd/internal/tap/l2l3"
)

var (
	// wintun proc 由 ensureWintun 显式绑定（见 wintun_embed_windows.go），
	// dll 内嵌进二进制，运行时释放并加载，部署为单文件。
	procWintunCreateAdapter *syscall.Proc
	procWintunOpenAdapter   *syscall.Proc
	procWintunCloseAdapter  *syscall.Proc
	procWintunStartSession  *syscall.Proc
	procWintunEndSession    *syscall.Proc
	procWintunGetReadWait   *syscall.Proc
	procWintunReceivePacket *syscall.Proc
	procWintunReleasePacket *syscall.Proc
	procWintunAllocSend     *syscall.Proc
	procWintunSendPacket    *syscall.Proc
)

const (
	wintunRingCapacity   = 1 << 22 // 4 MiB，必须为 2 的幂且 ∈ [0x40000, 0x4000000]
	errorNoMoreItems     = syscall.Errno(259)
	errorAccessViolation = syscall.Errno(0xC0000005) // STATUS_ACCESS_VIOLATION: Close/Read ????
	tunnelType           = "vrndis"
	// maxFrame 伪以太网帧上限（1500 MTU + 头部余量）。
	maxFrame = 65536
)

type windowsTAP struct {
	name     string
	adapter  uintptr
	session  uintptr
	readWait syscall.Handle

	// closed ?? Close() ???????;?? 1 ? Read/Write ?? io.EOF,
	// ????? wintun ??????(0xC0000005)
	closed uint32

	// L2↔L3 适配状态（由 SetPeerInfo 注入，握手后只读）。
	mu         sync.RWMutex
	localMAC   net.HardwareAddr
	gatewayMAC net.HardwareAddr
	gatewayIP  net.IP
	localIP    net.IP
	cache      *l2l3.Cache
	// arpInject 把构造的 ARP Reply 帧发回服务器（不进 wintun）。
	// 由 client 在握手后注入；未注入时 ARP 应答被丢弃（记录日志）。
	arpInject func(frame []byte)
}

// Open 打开（或创建）wintun 适配器；需要管理员权限与 wintun.dll。
func Open(name string) (Device, error) {
	// 内嵌 wintun.dll：自动释放到 exe 同目录（或 temp）并加载，无需随包附带 dll。
	if err := ensureWintun(); err != nil {
		return nil, err
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
		cache:    l2l3.NewCache(),
	}, nil
}

// SetPeerInfo 注入 Assignment 的对端信息（MAC/网关/IP），
// 供 L2↔L3 转换使用。握手完成、配置完 TAP 后由 client 调用。
func (t *windowsTAP) SetPeerInfo(localMAC, gatewayMAC net.HardwareAddr, localIP net.IP) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.localMAC = append([]byte(nil), localMAC...)
	t.gatewayMAC = append([]byte(nil), gatewayMAC...)
	t.localIP = append([]byte(nil), localIP...)
	t.gatewayIP = nil // 由 client 通过 SetGatewayIP 注入
	return nil
}

// SetGatewayIP 注入网关 IP（Assignment.Gateway）。
// 与 SetPeerInfo 分开，因为网关 IP 与网关 MAC 来自 Assignment 的不同字段。
func (t *windowsTAP) SetGatewayIP(gw net.IP) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.gatewayIP = append([]byte(nil), gw...)
}

// SetARPInject 注入 ARP 应答回传通道。
// 客户端把收到的 ARP Reply 帧经此回调写回 TCP 中继连接，
// 服务器 Linux 内核据此解析到本机 MAC。
func (t *windowsTAP) SetARPInject(fn func(frame []byte)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.arpInject = fn
}

func (t *windowsTAP) Name() string { return t.name }

// Read 上行：从 wintun 读裸 IP 包，封装成完整以太网帧。
func (t *windowsTAP) Read() ([]byte, error) {
	for {
		if atomic.LoadUint32(&t.closed) == 1 {
			return nil, io.EOF
		}
		var size uint32
		ptr, _, err := procWintunReceivePacket.Call(t.session, uintptr(unsafe.Pointer(&size)))
		if ptr == 0 {
			if err == errorNoMoreItems {
				syscall.WaitForSingleObject(t.readWait, syscall.INFINITE)
				continue
			}
			return nil, errEOF("receive", err)
		}
		raw := make([]byte, size)
		copy(raw, unsafe.Slice((*byte)(unsafe.Pointer(ptr)), int(size)))
		procWintunReleasePacket.Call(t.session, ptr)

		frame := t.wrapUpstream(raw)
		if frame == nil {
			continue // 非 IP 包，静默丢弃（wintun 不应产出）
		}
		return frame, nil
	}
}

// wrapUpstream 裸 IP 包 → 伪以太网帧。
func (t *windowsTAP) wrapUpstream(raw []byte) []byte {
	t.mu.RLock()
	srcMAC := t.localMAC
	gwIP := t.gatewayIP
	gwMAC := t.gatewayMAC
	cache := t.cache
	t.mu.RUnlock()

	if srcMAC == nil {
		// 未注入对端信息（握手未完成）：无法确定源 MAC，丢弃。
		return nil
	}
	return l2l3.WrapIP(raw, srcMAC, nil, gwIP, gwMAC, cache)
}

// Write 下行：以太网帧 → 裸 IP 包进 wintun；ARP 帧拦截处理。
func (t *windowsTAP) Write(frame []byte) error {
	if atomic.LoadUint32(&t.closed) == 1 {
		return io.EOF
	}
	if len(frame) > maxFrame {
		return nil // 超大帧静默丢弃
	}
	t.mu.RLock()
	localIP := t.localIP
	localMAC := t.localMAC
	cache := t.cache
	inject := t.arpInject
	t.mu.RUnlock()

	ipPayload, arpReply := l2l3.UnwrapIP(frame, localIP, localMAC, cache)
	if arpReply != nil {
		// ARP 应答：必须发回服务器（不进 wintun——wintun 是三层驱动，
		// 对 ARP 帧调用 WintunSendPacket 会被静默丢弃，这正是
		// Windows 客户端 ping 不通网关的根因之一）。
		if inject != nil {
			inject(arpReply)
		}
	}
	if ipPayload == nil {
		return nil // ARP 帧或非 IP 帧，无裸包可送 wintun
	}

	ptr, _, err := procWintunAllocSend.Call(t.session, uintptr(len(ipPayload)))
	if ptr == 0 {
		return errEOF("allocate send packet", err)
	}
	copy(unsafe.Slice((*byte)(unsafe.Pointer(ptr)), len(ipPayload)), ipPayload)
	procWintunSendPacket.Call(t.session, ptr)
	return nil
}

// SetMAC wintun 适配器 MAC 由驱动管理，此为空操作（记录语义在上层）。
func (t *windowsTAP) SetMAC(mac net.HardwareAddr) error {
	// wintun 网卡 MAC 由驱动随机生成，无法真正设置；
	// 但需记录隧道侧标识，供 l2l3 封装/ARP 应答使用。
	t.mu.Lock()
	t.localMAC = append(net.HardwareAddr(nil), mac...)
	t.mu.Unlock()
	return nil
}

func (t *windowsTAP) SetIPv4(ip net.IP, prefix int) error {
	mask := net.IP(net.CIDRMask(prefix, 32)).String()
	out, err := exec.Command("netsh", "interface", "ipv4", "set", "address",
		fmt.Sprintf("name=%s", t.name), "source=static",
		fmt.Sprintf("addr=%s", ip.String()), fmt.Sprintf("mask=%s", mask)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("netsh set address: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	// 记录本机 IP，供 l2l3 ARP 应答判断（ARP 请求的 TPA == localIP 时应答）。
	t.mu.Lock()
	t.localIP = append(net.IP(nil), ip.To4()...)
	t.mu.Unlock()
	return nil
}

// SetMTU 用 netsh 设置适配器 MTU（wintun 适配器支持任意外层 MTU）。
func (t *windowsTAP) SetMTU(mtu int) error {
	if mtu <= 0 {
		return nil
	}
	out, err := exec.Command("netsh", "interface", "ipv4", "set", "subinterface",
		fmt.Sprintf("interface=%s", t.name), fmt.Sprintf("mtu=%d", mtu), "store=persistent").CombinedOutput()
	if err != nil {
		return fmt.Errorf("netsh set mtu: %v (%s)", err, strings.TrimSpace(string(out)))
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

// AddDefaultRouteProtected 先把服务器端点钉在物理默认网关上，再安装隧道
// 默认路由；排除失败直接返回错误，拒绝安装默认路由（Windows 上若隧道
// 连接被自身默认路由劫持，可能掐断用户的 RDP 会话，故采取保守策略）。
func (t *windowsTAP) AddDefaultRouteProtected(gateway, exclude net.IP) error {
	if exclude != nil {
		if err := t.addExcludeRoute(exclude); err != nil {
			return fmt.Errorf("排除服务器端点路由失败，拒绝安装默认路由以保护隧道连接: %w", err)
		}
	}
	return t.AddDefaultRoute(gateway)
}

// addExcludeRoute 用 route add 把 server /32 指向物理默认网关（metric 1）。
// 物理网关须在隧道默认路由安装之前探测（Get-NetRoute 排序取最低 RouteMetric）。
func (t *windowsTAP) addExcludeRoute(server net.IP) error {
	server = server.To4()
	if server == nil {
		return fmt.Errorf("排除路由仅支持 IPv4: %v", server)
	}
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
		"Get-NetRoute -DestinationPrefix '0.0.0.0/0' | Sort-Object -Property RouteMetric | Select-Object -First 1 -ExpandProperty NextHop").Output()
	if err != nil {
		return fmt.Errorf("Get-NetRoute: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	gw := net.ParseIP(strings.TrimSpace(string(out)))
	if gw == nil || gw.IsUnspecified() {
		return fmt.Errorf("无物理默认网关（NextHop=%q），无法排除服务器端点", strings.TrimSpace(string(out)))
	}
	out, err = exec.Command("route", "add", server.String(), "mask", "255.255.255.255", gw.String(), "metric", "1").CombinedOutput()
	if err != nil {
		return fmt.Errorf("route add exclude %s: %v (%s)", server, err, strings.TrimSpace(string(out)))
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

// errEOF ? wintun syscall ????? io.EOF ??,???(0xC0000005)
// ???????;????? devToSock ???"??"????,?? panic/???
func errEOF(op string, err error) error {
	if err == nil {
		return nil
	}
	if errno, ok := err.(syscall.Errno); ok && errno == errorAccessViolation {
		return fmt.Errorf("wintun: %s: %w (access violation)", op, io.EOF)
	}
	return fmt.Errorf("wintun: %s: %w: %v", op, io.EOF, err)
}

// Close ??????;???? closed ???,?? Read/Write ???
// ???,???????????(0xC0000005)
func (t *windowsTAP) Close() error {
	if !atomic.CompareAndSwapUint32(&t.closed, 0, 1) {
		return nil // ???
	}
	if t.session != 0 {
		procWintunEndSession.Call(t.session)
	}
	if t.adapter != 0 {
		procWintunCloseAdapter.Call(t.adapter)
	}
	return nil
}
