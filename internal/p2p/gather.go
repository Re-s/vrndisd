package p2p

import (
	"fmt"
	"net"
	"strings"
)

// lanIfPrefixes 不作为 LAN 直连候选来源的接口名前缀：
//   - 容器/虚拟网桥：地址对同 NAT 其它设备不可达
//   - tun/tap/wintun/utun/ppp/vrndis：VPN/隧道接口，注意 Linux TAP 不设
//     FlagPointToPoint（与 PPP 等不同），必须靠名字与网段双重排除，
//     否则本机其它隧道网段地址会混入废候选。
var lanIfPrefixes = []string{"docker", "br-", "veth", "virbr", "ovs-", "tunl", "flannel",
	"cni", "podman", "vrndis", "tun", "tap", "wintun", "utun", "ppp", "wg", "ipip", "sit", "gre", "he-"}

// HostCandidates 收集本机可作为 LAN 直连 host 候选的地址（"ip:port" 形式）：
// IPv4 私网地址（RFC1918）——同 NAT 设备内网直连，绕开 NAT 无 hairpin 的死路。
//
// localPort 为本机打洞 socket 实际监听端口（与 srflx 候选共用 socket），
// excludeIP 为本机隧道 IP，tunnelNet 为本机隧道网段（两者整体排除：
// 隧道网段地址发给对端只能走中继/落空，是废候选）。
//
// 不收集 IPv6：打洞通路（Discover/PunchConn）为兼容 Windows 强制 udp4
// socket（AF_INET6 dual-stack 上偶发收不到 v4-mapped 响应），v6 候选
// 对 udp4 socket 写不出、对端对它只能超时——是纯噪声。
// v6 直连需 dual-stack 打洞通路整体改造（Discover/Tunnel 读写与
// Session 地址族统一），留作后续提升项。
//
// v4 公网地址不在此列：物理网卡上的公网 v4 与 srflx 候选语义重复，
// 且对多数客户端无独立可达价值。
func HostCandidates(localPort int, excludeIP net.IP, tunnelNet *net.IPNet) ([]string, error) {
	if localPort <= 0 || localPort > 65535 {
		return nil, fmt.Errorf("vrndis/p2p: bad local port %d", localPort)
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("vrndis/p2p: list interfaces: %w", err)
	}
	var out []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		// tun/tap/wintun/PPP 等点对点接口：本机隧道接口（含 vrndis TAP）
		// 落在此类，其地址不是物理可达的 LAN 端点。
		if ifc.Flags&net.FlagPointToPoint != 0 {
			continue
		}
		name := strings.ToLower(ifc.Name)
		skip := false
		for _, p := range lanIfPrefixes {
			if strings.HasPrefix(name, p) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipNet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipNet.IP
			ip4 := ip.To4()
			if ip4 == nil {
				continue // 不收集 v6（见函数注释）
			}
			// v4：仅私网（RFC1918；CGNAT 100.64/10 是运营商级，
			// 同站内通常不可达且 IsPrivate 不含它，单独排除）
			if !ip4.IsPrivate() || (ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127) {
				continue
			}
			if !ip.IsGlobalUnicast() {
				continue
			}
			if excludeIP != nil && ip.Equal(excludeIP) {
				continue
			}
			if tunnelNet != nil && tunnelNet.Contains(ip) {
				continue
			}
			out = append(out, net.JoinHostPort(ip.String(), fmt.Sprint(localPort)))
		}
	}
	return out, nil
}
