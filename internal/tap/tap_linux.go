//go:build linux

package tap

import (
	"fmt"
	"net"
	"os/exec"
	"syscall"
	"unsafe"
)

const (
	tunSetIFF      = 0x400454CA
	iffTap         = 0x0002
	iffNoPI        = 0x1000
	siocGifFlags   = 0x8913
	siocSifFlags   = 0x8914
	siocSifHWAddr  = 0x8924
	siocSifAddr    = 0x8916
	siocSifNetmask = 0x891C
	arphrdEther    = 1
	iffUp          = 0x1
	ifreqSize      = 40
)

// linuxTAP 真实 TAP 网卡。
//
// 注意：读写走原生阻塞 syscall（而非 os.File），刻意绕开 Go 运行时 netpoller
// —— /dev/net/tun 在部分内核/时序下会触发 "not pollable" 错误。
// 阻塞读由专用协程承担，代价是占一个 OS 线程，行为可预期。
type linuxTAP struct {
	fd   int
	name string
	ctl  int // AF_INET 数据报套接字，专用于接口配置 ioctl
}

// Open 打开（或创建）一块真实 Linux TAP 网卡，需要 root 与 /dev/net/tun。
func Open(name string) (Device, error) {
	fd, err := syscall.Open("/dev/net/tun", syscall.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: open /dev/net/tun: %v (需要 root 且内核启用 TUN/TAP)", ErrUnsupported, err)
	}
	var ifr [ifreqSize]byte
	copy(ifr[:15], name)
	*(*uint16)(unsafe.Pointer(&ifr[16])) = iffTap | iffNoPI
	if err := ioctl(uintptr(fd), tunSetIFF, &ifr[0]); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("create TAP %q: %w", name, err)
	}
	actual := cstr(ifr[:16])

	ctl, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		syscall.Close(fd)
		return nil, err
	}
	return &linuxTAP{fd: fd, name: actual, ctl: ctl}, nil
}

func (t *linuxTAP) Name() string { return t.name }

func (t *linuxTAP) Read() ([]byte, error) {
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

func (t *linuxTAP) Write(frame []byte) error {
	for {
		_, err := syscall.Write(t.fd, frame)
		if err == syscall.EINTR {
			continue
		}
		return err
	}
}

func (t *linuxTAP) SetMAC(mac net.HardwareAddr) error {
	var ifr [ifreqSize]byte
	copy(ifr[:15], t.name)
	*(*uint16)(unsafe.Pointer(&ifr[16])) = arphrdEther
	copy(ifr[18:24], mac)
	return ioctl(uintptr(t.ctl), siocSifHWAddr, &ifr[0])
}

func (t *linuxTAP) SetIPv4(ip net.IP, prefix int) error {
	ip4 := ip.To4()
	if ip4 == nil {
		return fmt.Errorf("vrndis/tap: not an IPv4 address: %v", ip)
	}
	// SIOCSIFADDR: sockaddr_in {family u16, port u16, addr [4]byte}
	var ifr [ifreqSize]byte
	copy(ifr[:15], t.name)
	*(*uint16)(unsafe.Pointer(&ifr[16])) = syscall.AF_INET
	copy(ifr[20:24], ip4)
	if err := ioctl(uintptr(t.ctl), siocSifAddr, &ifr[0]); err != nil {
		return err
	}
	var mask [ifreqSize]byte
	copy(mask[:15], t.name)
	*(*uint16)(unsafe.Pointer(&mask[16])) = syscall.AF_INET
	copy(mask[20:24], net.CIDRMask(prefix, 32))
	return ioctl(uintptr(t.ctl), siocSifNetmask, &mask[0])
}

func (t *linuxTAP) Up() error {
	var ifr [ifreqSize]byte
	copy(ifr[:15], t.name)
	if err := ioctl(uintptr(t.ctl), siocGifFlags, &ifr[0]); err != nil {
		return err
	}
	flags := *(*uint16)(unsafe.Pointer(&ifr[16])) | iffUp
	*(*uint16)(unsafe.Pointer(&ifr[16])) = flags
	return ioctl(uintptr(t.ctl), siocSifFlags, &ifr[0])
}

func (t *linuxTAP) AddDefaultRoute(gateway net.IP) error {
	cmd := exec.Command("ip", "route", "replace", "default", "via", gateway.String(), "dev", t.name)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ip route replace default: %v (%s)", err, out)
	}
	return nil
}

func (t *linuxTAP) Close() error {
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
