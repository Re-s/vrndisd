//go:build android

package tap

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

const (
	androidTunSetIFF     = 0x400454CA
	androidIffTap        = 0x0002
	androidIffNoPI       = 0x1000
	androidSiocGifFlags  = 0x8913
	androidSiocSifFlags  = 0x8914
	androidSiocSifHWAddr = 0x8924
	androidArphrdEther   = 1
	androidIffUp         = 0x1
	androidIfreqSize     = 40
)

// androidTAP 基于 /dev/net/tun 的 TAP 网卡（Android 需 root 且内核启用 tun）。
//
// 与 Linux 实现的差异在地址/路由配置：Android 不使用裸 ioctl 设地址
// （SIOCSIFADDR 不会自动建立直连路由，且不带 netd 网络命名空间/路由表上下文），
// 而是走 toybox `ip addr add <ip>/<prefix> dev <tap>` 自动建 connected 路由，
// 再用 `ip route add <net>/<prefix> dev <tap> scope link` 与带 onlink 的默认路由。
//
// 关键差异——netd fwmark 绕过：Android netd 用 ip rule 把本地流量按 uid/oif
// 锁进物理网卡专用的路由表（如 11000/17000: iif lo oif wlan0 lookup 1009），
// main 表里的隧道直连路由根本查不到，ping 网关会被表 1009 的默认路由劫持去
// wlan0。系统给桥接网络（如 ds-br0）的现成范式是在低 prio 插 to 规则查 main：
// `ip rule add to <net> lookup main prio 6091`，先于 11000 命中，隧道流量回到
// main 表。规则命中但表内无路由时会 fall through 到下一条规则，故网卡关闭后
// 残留的 to 规则安全（恢复物理路径）。
type androidTAP struct {
	fd     int
	name   string
	ctl    int
	subnet *net.IPNet // SetIPv4 记录的直连网段，供 Up() 建立 scope link 子网路由
}

// Open 创建 Android TAP。沿用 Linux 的 ioctl 建接口，但配置走 ip 命令。
func Open(name string) (Device, error) {
	fd, err := syscall.Open("/dev/net/tun", syscall.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: open /dev/net/tun: %v (需要 root 且内核启用 TUN/TAP)", ErrUnsupported, err)
	}
	var ifr [androidIfreqSize]byte
	copy(ifr[:15], name)
	*(*uint16)(unsafe.Pointer(&ifr[16])) = androidIffTap | androidIffNoPI
	if err := ioctl(uintptr(fd), androidTunSetIFF, &ifr[0]); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("create TAP %q: %w", name, err)
	}
	actual := cstr(ifr[:16])

	ctl, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		syscall.Close(fd)
		return nil, err
	}
	return &androidTAP{fd: fd, name: actual, ctl: ctl}, nil
}

func (t *androidTAP) Name() string { return t.name }

func (t *androidTAP) Read() ([]byte, error) {
	buf := make([]byte, 65536)
	for {
		n, err := syscall.Read(t.fd, buf)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return nil, err
		}
		return buf[:n], nil
	}
}

func (t *androidTAP) Write(frame []byte) error {
	for {
		_, err := syscall.Write(t.fd, frame)
		if err == syscall.EINTR {
			continue
		}
		return err
	}
}

func (t *androidTAP) SetMAC(mac net.HardwareAddr) error {
	var ifr [androidIfreqSize]byte
	copy(ifr[:15], t.name)
	*(*uint16)(unsafe.Pointer(&ifr[16])) = androidArphrdEther
	copy(ifr[18:24], mac)
	return ioctl(uintptr(t.ctl), androidSiocSifHWAddr, &ifr[0])
}

// SetIPv4 用 ip 命令配置地址（而非裸 ioctl）：
//   - 先 `ip -4 addr flush` 清掉网卡全部 IPv4（含旧 ASSIGN 残留的
//     secondary），再 add 目标地址——全量设置语义，重连换 IP 时旧地址
//     不堆积（曾实测一张卡积累 8 个地址而内核只用主地址一个）。
//     -4 只清 IPv4，链路本地 v6 由内核管理不受影响。
//   - 显式 scope link 子网路由放到 Up() 里建立（见 Up），保证网卡已 up；
//     子网网段不变，路由跨重连复用（Up 已容错 File exists）。
func (t *androidTAP) SetIPv4(ip net.IP, prefix int) error {
	ip4 := ip.To4()
	if ip4 == nil {
		return fmt.Errorf("github.com/Re-s/vrndisd/tap: not an IPv4 address: %v", ip)
	}
	addr := (&net.IPNet{IP: ip4, Mask: net.CIDRMask(prefix, 32)}).String()
	_ = exec.Command("ip", "-4", "addr", "flush", "dev", t.name).Run()
	if out, err := exec.Command("ip", "addr", "add", addr, "dev", t.name).CombinedOutput(); err != nil {
		// flush 与 add 之间的竞争（极偶发）仍可能 File exists，属幂等结果。
		if !strings.Contains(string(out), "File exists") {
			return fmt.Errorf("ip addr add: %v (%s)", err, out)
		}
	}
	t.subnet = &net.IPNet{IP: ip4.Mask(net.CIDRMask(prefix, 32)), Mask: net.CIDRMask(prefix, 32)}
	return nil
}

// SetMTU 用 ip 命令设置网卡 MTU（Android 无 SIOCSIFMTU 权限，走 ip link）。
func (t *androidTAP) SetMTU(mtu int) error {
	if mtu <= 0 {
		return nil
	}
	out, err := exec.Command("ip", "link", "set", "dev", t.name, "mtu", strconv.Itoa(mtu)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip link set mtu: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (t *androidTAP) Up() error {
	var ifr [androidIfreqSize]byte
	copy(ifr[:15], t.name)
	if err := ioctl(uintptr(t.ctl), androidSiocGifFlags, &ifr[0]); err != nil {
		return err
	}
	flags := *(*uint16)(unsafe.Pointer(&ifr[16])) | androidIffUp
	*(*uint16)(unsafe.Pointer(&ifr[16])) = flags
	if err := ioctl(uintptr(t.ctl), androidSiocSifFlags, &ifr[0]); err != nil {
		return err
	}
	// 网卡已 up，锚定直连（scope link）子网路由，让网关落在直连网段、可做 on-link/ARP 判定。
	if t.subnet != nil {
		if out, err := exec.Command("ip", "route", "add", t.subnet.String(),
			"dev", t.name, "scope", "link").CombinedOutput(); err != nil {
			if !strings.Contains(string(out), "File exists") {
				return fmt.Errorf("ip route add subnet: %v (%s)", err, out)
			}
		}
		// netd fwmark 绕过：详见类型注释。失败仅记录（pref pref）不阻断，
		// 部分无该机制 ROM 上不影响其余路径。
		if out, err := exec.Command("ip", "rule", "add", "to", t.subnet.String(),
			"lookup", "main", "prio", "6091").CombinedOutput(); err != nil {
			if !strings.Contains(string(out), "File exists") {
				fmt.Fprintf(os.Stderr, "github.com/Re-s/vrndisd/tap: ip rule add (netd 绕过) 失败: %v (%s)\n", err, out)
			}
		}
	}
	return nil
}

// AddDefaultRoute 与 Linux 一致：网关锚直连 + 默认路由带 onlink。
func (t *androidTAP) AddDefaultRoute(gateway net.IP) error {
	if gw := gateway.To4(); gw != nil {
		if out, err := exec.Command("ip", "route", "add", gw.String(),
			"dev", t.name, "scope", "link").CombinedOutput(); err != nil {
			if !strings.Contains(string(out), "File exists") {
				return fmt.Errorf("ip route add gateway: %v (%s)", err, out)
			}
		}
	}
	cmd := exec.Command("ip", "route", "replace", "default",
		"via", gateway.String(), "dev", t.name, "onlink", "metric", "100")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ip route replace default (onlink): %v (%s)", err, out)
	}
	return nil
}

// SetPeerInfo Android TAP 是真二层网卡，无需 L2↔L3 转换；
// 接口实现为空操作。
func (t *androidTAP) SetPeerInfo(localMAC, gatewayMAC net.HardwareAddr, localIP net.IP) error {
	return nil
}

func (t *androidTAP) Close() error {
	// 清理 netd 绕过规则（残留也安全——表内无路由会 fall through，
	// 删除仅为整洁）。失败忽略（网卡可能已不在）。
	if t.subnet != nil {
		_ = exec.Command("ip", "rule", "del", "to", t.subnet.String(),
			"lookup", "main", "prio", "6091").Run()
	}
	syscall.Close(t.ctl)
	return syscall.Close(t.fd)
}

func ioctl(fd, req uintptr, arg *byte) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(unsafe.Pointer(arg)))
	if errno != 0 {
		return errno
	}
	return nil
}

func cstr(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}
