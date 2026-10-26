package p2p

// 网关端口映射（UPnP IGD / NAT-PMP），用于在 NAT 上打一个显式 full-cone
// 映射作为 P2P 候选。这是对称 NAT 环境提升直连率的最大杠杆（n2n 社区
// 实测：无映射 7%-11%，启用 UPnP 后 89%）：
//
//   - UPnP IGD：SSDP 发现网关 -> SOAP AddPortMapping
//   - NAT-PMP（RFC6886）：网关 UDP 5351 上的简单二进制映射协议，
//     外部端口由网关分配
//
// 全部失败时返回 0，调用方静默跳过（无 UPnP 的网络照常工作于纯 STUN 打洞）。
// 对称 NAT 打洞失败的根因是"对端猜不到本端端口号"，显式映射让本端端口
// 固定可猜，从而救活"一侧对称 NAT"的组合。

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	portmapTimeout = 3 * time.Second // 单次映射尝试上限（打洞总预算内的小窗口）
	pmpPort        = 5351            // NAT-PMP/PCP 网关端口
	pmpLifetime    = 3600            // 映射 TTL（秒），客户端每次打洞轮都会重续
	upnpLease      = 3600
)

var (
	// UPnP IGD 描述里找 controlURL 与 serviceType。IGD 描述文档良莠不齐
	// （混杂 HTML、未闭合标签、命名空间变体），encoding/xml 解析容易翻车，
	// 业界实现（miniupnpc 等）普遍用正则，这里同。
	reServiceType = regexp.MustCompile(`<(?:&)??serviceType[^>]*>([^<]+)</(?:&)??serviceType`)
	reControlURL  = regexp.MustCompile(`<(?:&)??controlURL[^>]*>([^<]+)</(?:&)??controlURL`)
	ssdpST        = "urn:schemas-upnp-org:device:InternetGatewayDevice:1"
)

// localOutbound 返回本机默认路由出口的接口地址（UDP "连接" 8.8.8.8 不发包，
// 仅让内核选路并填本地地址；离线/无默认路由时返回错误）。
func localOutbound() (net.IP, error) {
	conn, err := net.DialTimeout("udp", "8.8.8.8:53", portmapTimeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP, nil
}

// guessGateway 按出口接口地址推断网关：取同网段末位 .1（/24 假设对绝大
// 多数家庭网络成立；OpenWrt/小米/TP-Link 默认网关皆为 x.x.x.1）。
func guessGateway(localIP net.IP) net.IP {
	if ip4 := localIP.To4(); ip4 != nil {
		if ip4[3] == 1 {
			return ip4 // 本机就是 .1（旁路由/网关本机）
		}
		return net.IPv4(ip4[0], ip4[1], ip4[2], 1).To4()
	}
	return nil
}

// MapPort 尝试为 localPort 建立网关端口映射，返回外部端口（0=失败）。
// 顺序：UPnP IGD（支持率最高）-> NAT-PMP。两者都静默尝试，互不干扰。
func MapPort(localPort int, logger *log.Logger) int {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	if port := tryUPnP(localPort, logger); port != 0 {
		logger.Printf("github.com/Re-s/vrndisd/p2p: UPnP 映射成功 -> 外部端口 %d", port)
		return port
	}
	if port := tryPMP(localPort, logger); port != 0 {
		logger.Printf("github.com/Re-s/vrndisd/p2p: NAT-PMP 映射成功 -> 外部端口 %d", port)
		return port
	}
	return 0
}

// ---- NAT-PMP（RFC6886）----

// tryPMP 向网关请求 UDP 映射：externalPort(建议=localPort) -> localPort。
// PMP 响应带回网关实际分配的外部端口（路由器可改配）。
func tryPMP(localPort int, logger *log.Logger) int {
	localIP, err := localOutbound()
	if err != nil {
		return 0
	}
	gw := guessGateway(localIP)
	if gw == nil {
		return 0
	}
	return pmpMap(&net.UDPAddr{IP: gw, Port: pmpPort}, localPort)
}

// pmpMap 向指定网关请求 NAT-PMP UDP 映射。拆出网关地址（含端口）以便
// 测试注入 fake 网关。
func pmpMap(gw *net.UDPAddr, localPort int) int {
	conn, err := net.DialUDP("udp4", nil, gw)
	if err != nil {
		return 0
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(portmapTimeout))
	// op=1 映射请求: ver(0) op(1) internalPort(u16) suggestedExternalPort(u16) lifetime(u32)
	req := make([]byte, 12)
	binary.BigEndian.PutUint16(req[2:], uint16(localPort))
	binary.BigEndian.PutUint16(req[4:], uint16(localPort))
	binary.BigEndian.PutUint32(req[8:], pmpLifetime)
	if _, err := conn.Write(req); err != nil {
		return 0
	}
	buf := make([]byte, 16)
	n, err := conn.Read(buf)
	if err != nil || n < 12 {
		return 0
	}
	if buf[0] != 0 || buf[1] != 128+1 { // 响应 op = 0x81
		return 0
	}
	if result := binary.BigEndian.Uint16(buf[2:4]); result != 0 {
		// 常见 3=未授权/4=资源不足/5=不支持，静默
		return 0
	}
	// RFC 6886 §3.5: ver(1) op(1) result(2) epoch(4) internalPort(2)
	//               externalPort(2) lifetime(4)
	return int(binary.BigEndian.Uint16(buf[10:12])) // 外部端口
}

// ---- UPnP IGD ----

type igdService struct {
	controlURL string // POST 目标（绝对地址）
	serviceURI string // SOAPAction 的 service type（含版本号）
}

// ssdpDiscover 发送 M-SEARCH 找 IGD，返回其描述文档 LOCATION。
func ssdpDiscover(ctx context.Context) (string, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return "", err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(portmapTimeout))
	msg := "M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\nMAN: \"ssdp:discover\"\r\nMX: 2\r\nST: " + ssdpST + "\r\n\r\n"
	if _, err := conn.WriteToUDP([]byte(msg), &net.UDPAddr{IP: net.ParseIP("239.255.255.250"), Port: 1900}); err != nil {
		return "", err
	}
	buf := make([]byte, 4096)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			return "", err
		}
		if loc := locateHeader(buf[:n]); loc != "" {
			return loc, nil
		}
	}
}

func locateHeader(resp []byte) string {
	for _, line := range bytes.Split(resp, []byte("\r\n")) {
		s := string(line)
		if len(s) > 9 && strings.EqualFold(s[:9], "LOCATION:") {
			return strings.TrimSpace(s[9:])
		}
	}
	return ""
}

// igdServices 从 IGD 描述文档解析可用控制端点。WANIPConnection 为现代
// 路由器标配；WANPPPConnection 为老式 PPPoE 设备回退。
func igdServices(descURL string) []igdService {
	req, err := http.NewRequest("GET", descURL, nil)
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), portmapTimeout)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil || resp == nil {
		return nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil
	}
	want := []string{
		"urn:schemas-upnp-org:service:WANIPConnection:1",
		"urn:schemas-upnp-org:service:WANIPConnection:2",
		"urn:schemas-upnp-org:service:WANPPPConnection:1",
	}
	var out []igdService
	types := reServiceType.FindAllSubmatch(body, -1)
	urls := reControlURL.FindAllSubmatch(body, -1)
	// serviceType 与 controlURL 在描述文档里成对出现（同一个 <service> 内），
	// 两次正则的序列顺序一致时下标对齐。
	for i, st := range types {
		if i >= len(urls) {
			break
		}
		stype := string(bytes.TrimSpace(st[1]))
		ctrl := string(bytes.TrimSpace(urls[i][1]))
		for _, w := range want {
			if stype == w {
				if !strings.Contains(ctrl, "://") {
					// 相对路径：拼接描述文档的 scheme://host
					base := descURL
					if i := strings.Index(base, "://"); i >= 0 {
						base = base[:strings.IndexByte(base[i+3:], '/')+i+3]
					}
					ctrl = base + ctrl
				}
				out = append(out, igdService{controlURL: ctrl, serviceURI: stype})
			}
		}
	}
	return out
}

// tryUPnP 走 SSDP -> 描述文档 -> SOAP AddPortMapping 建立映射。
// NewInternalClient 用本机出口接口地址（同网段可直接到达网关）。
func tryUPnP(localPort int, logger *log.Logger) int {
	ctx, cancel := context.WithTimeout(context.Background(), portmapTimeout)
	defer cancel()
	loc, err := ssdpDiscover(ctx)
	if err != nil || loc == "" {
		return 0
	}
	localIP, err := localOutbound()
	if err != nil {
		return 0
	}
	for _, svc := range igdServices(loc) {
		port := upnpAddMapping(svc, localIP, localPort)
		if port != 0 {
			return port
		}
	}
	return 0
}

// upnpAddMapping 向一个 IGD 控制端点发 AddPortMapping SOAP。
// 请求外部端口 = localPort（路由器可拒绝并回 500，试下一个 service）。
func upnpAddMapping(svc igdService, internalIP net.IP, localPort int) int {
	envelope := fmt.Sprintf(`<?xml version="1.0"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">
<s:Body>
<u:AddPortMapping xmlns:u="%s">
<NewRemoteHost></NewRemoteHost>
<NewExternalPort>%d</NewExternalPort>
<NewProtocol>UDP</NewProtocol>
<NewInternalPort>%d</NewInternalPort>
<NewInternalClient>%s</NewInternalClient>
<NewEnabled>1</NewEnabled>
<NewPortMappingDescription>vrndis</NewPortMappingDescription>
<NewLeaseDuration>%d</NewLeaseDuration>
</u:AddPortMapping>
</s:Body>
</s:Envelope>`, svc.serviceURI, localPort, localPort, internalIP.String(), upnpLease)

	req, err := http.NewRequest("POST", svc.controlURL, strings.NewReader(envelope))
	if err != nil {
		return 0
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPAction", `"`+svc.serviceURI+`#AddPortMapping"`)
	ctx, cancel := context.WithTimeout(context.Background(), portmapTimeout)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 {
		return localPort
	}
	return 0
}
