//go:build android

package tap

import (
	"fmt"
	"net"
	"os/exec"
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
//   - `ip addr add <ip>/<prefix> dev <tap>` 分配地址（网卡 up 时内核会自动建 connected 子网路由）。
//   - 显式 scope link 子网路由放到 Up() 里建立（见 Up），保证网卡已 up。
func (t *androidTAP) SetIPv4(ip net.IP, prefix int) error {
	ip4 := ip.To4()
	if ip4 == nil {
		return fmt.Errorf("vrndis/tap: not an IPv4 address: %v", ip)
	}
	addr := (&net.IPNet{IP: ip4, Mask: net.CIDRMask(prefix, 32)}).String()
	if out, err := exec.Command("ip", "addr", "add", addr, "dev", t.name).CombinedOutput(); err != nil {
		// 地址已存在（重连复用）属幂等，忽略。
		if !strings.Contains(string(out), "File exists") {
			return fmt.Errorf("ip addr add: %v (%s)", err, out)
		}
	}
	t.subnet = &net.IPNet{IP: ip4.Mask(net.CIDRMask(prefix, 32)), Mask: net.CIDRMask(prefix, 32)}
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
