package p2p

import (
	"fmt"
	"net"
	"strings"
)

// lanIfPrefixes 不作为 LAN 直连候选来源的接口名前缀：
// 容器/虚拟网桥上的地址对同 NAT 其它设备不可达，收集只制造废候选
// （点对点隧道接口由 FlagPointToPoint 过滤，本机隧道网段由 excludeIP 排除）。
var lanIfPrefixes = []string{"docker", "br-", "veth", "virbr", "ovs-", "tunl", "flannel", "cni", "podman"}

// HostCandidates 收集本机可作为 LAN 直连 host 候选的地址（"ip:port" 形式）：
//   - IPv4 私网地址（RFC1918）：同 NAT 设备内网直连，绕开 NAT 无 hairpin 的死路。
//   - IPv6 global unicast 且非 ULA（fc00::/7）：公网 v6 直连完全绕开 NAT。
//
// localPort 为本机打洞 socket 实际监听端口（与 srflx 候选共用 socket），
// excludeIP 为本机隧道 IP（排除后等价于排除 10.77.x 隧道网段候选——
// 该地址在 TAP 接口上，对端发包只能绕中继，是废候选）。
//
// v4 公网地址不在此列：物理网卡上的公网 v4 与 srflx 候选语义重复，
// 且对多数客户端无独立可达价值。
func HostCandidates(localPort int, excludeIP net.IP) ([]string, error) {
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
			if !ip.IsGlobalUnicast() {
				continue
			}
			if ip4 := ip.To4(); ip4 != nil {
				// v4：仅私网（RFC1918；CGNAT 100.64/10 是运营商级，
				// 同站内通常不可达且 IsPrivate 不含它，单独排除）
				if !ip4.IsPrivate() || (ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127) {
					continue
				}
			} else if ip.IsPrivate() {
				// v6 ULA（fc00::/7）：站点本地，跨 NAT 不可路由，排除
				continue
			}
			if excludeIP != nil && ip.Equal(excludeIP) {
				continue
			}
			out = append(out, net.JoinHostPort(ip.String(), fmt.Sprint(localPort)))
		}
	}
	return out, nil
}
