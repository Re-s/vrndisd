//go:build linux && !android

package tap

import (
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestSetIPv4ReconnectNoLeak 模拟重连场景：同一网卡两次 SetIPv4
// （两次 ASSIGN 分配到不同 IP），网卡上应只保留最后一个 IPv4，
// 不能堆积 secondary（真机症状：一张卡积累 8 个地址）。
// 需 root 与 /dev/net/tun，否则跳过。
func TestSetIPv4ReconnectNoLeak(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("需要 root 创建 TAP")
	}
	if _, err := os.Stat("/dev/net/tun"); err != nil {
		t.Skip("无 /dev/net/tun")
	}
	const ifname = "vrndis-fb1"
	dev, err := Open(ifname)
	if err != nil {
		t.Fatalf("open tap: %v", err)
	}
	defer dev.Close()

	if err := dev.SetIPv4(net.ParseIP("10.77.0.9"), 24); err != nil {
		t.Fatalf("第一次 SetIPv4: %v", err)
	}
	if err := dev.SetIPv4(net.ParseIP("10.77.0.8"), 24); err != nil {
		t.Fatalf("第二次 SetIPv4: %v", err)
	}

	out, err := exec.Command("ip", "-4", "addr", "show", "dev", ifname).CombinedOutput()
	if err != nil {
		t.Fatalf("ip addr show: %v (%s)", err, out)
	}
	inets := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "inet ") {
			inets++
		}
	}
	if inets != 1 {
		t.Errorf("IPv4 地址泄漏：期望 1 个，实际 %d 个:\n%s", inets, out)
	}
}
