// vrndisd —— 基于 IP(TCP) 连接的虚拟 RNDIS 隧道（服务器/客户端/服务管理）。
//
// 用法：
//
//	vrndisd server  [--listen 0.0.0.0:7777] [--network 10.77.0.0/24] [--gateway 10.77.0.1] [--stun] [--stun-listen :5478] ...
//	vrndisd client  [--server HOST:7777] [--tap vrndis-c0] [--name X] [--default-route]
//	vrndisd service install|uninstall|start|stop|status [--mode server|client ...]
//	vrndisd service-run [--config PATH]
//	vrndisd stun-server [--listen :5478] [--version]
//	vrndisd status
//	vrndisd clients
//	vrndisd help
//	vrndisd version
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Re-s/vrndisd/internal/client"
	"github.com/Re-s/vrndisd/internal/ctrl"
	"github.com/Re-s/vrndisd/internal/loglevel"
	"github.com/Re-s/vrndisd/internal/netcheck"
	"github.com/Re-s/vrndisd/internal/proto"
	"github.com/Re-s/vrndisd/internal/server"
	"github.com/Re-s/vrndisd/internal/service"
	stunserver "github.com/Re-s/vrndisd/internal/stun/server"
)

var Version = "dev"

func main() {
	log.SetFlags(log.LstdFlags)
	os.Exit(run(os.Args[1:]))
}

// run 解析参数并执行对应子命令，返回进程退出码。独立成函数以便单测无参数分支。
func run(argv []string) int {
	args, globalCfg := extractGlobalConfig(argv)
	if len(args) < 1 {
		usage()
		return 2
	}
	var err error
	switch args[0] {
	case "server":
		err = cmdServer(args[1:])
	case "client":
		err = cmdClient(args[1:])
	case "service":
		err = cmdService(args[1:])
	case "stun-server", "stun":
		err = cmdStunServer(args[1:])
	case "netcheck":
		err = cmdNetcheck(args[1:])
	case "ping":
		err = cmdPing(args[1:])
	case "peers":
		err = cmdPeers(args[1:])
	case "status":
		err = cmdStatus(args[1:])
	case "clients":
		err = cmdClients(args[1:])
	case "local":
		err = cmdLocal(args[1:])
	case "service-run":
		err = cmdServiceRun(args[1:], globalCfg)
	case "version", "-V", "--version":
		fmt.Printf("vrndisd %s (%s/%s)\n", Version, runtime.GOOS, runtime.GOARCH)
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		return 2
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "[vrndisd] 错误: %v\n", err)
		return 1
	}
	return 0
}

func usage() {
	fmt.Printf(`vrndisd - 基于 IP(TCP) 连接的虚拟 RNDIS 隧道（版本 %s）

命令：
  server         启动服务器（分配地址 + 帧转发 + 网关）
  client         连接服务器，获得分配地址并建立本地虚拟网卡
  service        持久化服务管理：install|uninstall|start|stop|status|export
  service-run    服务入口（由 systemd/SCM 调用，也可前台运行）
  stun-server    内置 STUN 服务器（RFC 3489/5389 Binding，供 NAT 穿透）
  netcheck       探测本机 NAT 环境与 UDP 可达性（P2P 打洞能力评估）
  ping           探测到指定 peer 的路径（relay/p2p + RTT，tailscale 风格）
  peers          列出全部在线 peer 及各自路径状态
  status         服务器运行状态与在线客户端概况
  clients        列出当前在线客户端（名称/IP/MAC/对端）
  local          列出本机全部 vrndisd 实例（扫描本机管理端口 7790..7799）
  version        显示版本号
  help           显示本帮助

通用选项（server / client / service install / service-run）：
  --log-level <debug|info|warn|error>   日志级别，默认 info
      debug 含数据面逐帧事件（网卡队列满丢帧、P2P 动态路由注册、候选变化），
      弱网/弱设备下会刷屏；逐帧事件已自动合并为每秒一条计数统计。
      服务模式也可写进配置文件的 "log_level" 字段（service install 持久化），
      service-run 的 --log-level 可临时覆盖配置文件。

示例（日志）：
  sudo vrndisd server --listen 0.0.0.0:7777 --log-level warn
  sudo vrndisd client --server 1.2.3.4:7777 --log-level debug
  sudo vrndisd service install --mode client --server 1.2.3.4:7777 --log-level warn

示例（STUN）：
  sudo vrndisd server --listen 0.0.0.0:7777 --stun
  sudo vrndisd server --listen 0.0.0.0:7777 --stun --stun-listen 0.0.0.0:5478
  sudo vrndisd stun-server --listen 0.0.0.0:5478
  vrndisd stun-server --version

示例（netcheck）：
  vrndisd netcheck                                       # 自动用已安装配置连接的服务器探测
  vrndisd netcheck --server 1.2.3.4                      # 指定服务器双端口探测
  vrndisd netcheck --server 1.2.3.4 --extra-stun stun.l.google.com:19302

示例（ping / peers）：
  vrndisd ping 10.77.0.4                      # 免输：自动读取已安装配置的服务器/PSK
  vrndisd ping 10.77.0.4 --server 1.2.3.4:7777 --psk yourpsk
  vrndisd peers                              # 同上，列出全部在线 peer
  vrndisd peers --server 1.2.3.4:7777 --psk yourpsk

示例（管理查询，服务器或客户端本机执行）：
  vrndisd status                             # 状态：版本 + 在线客户端数
  vrndisd clients                            # 在线客户端：地址/名称/MAC/对端/P2P 路径
  vrndisd local                              # 本机全部实例（server + 每个client）
  vrndisd local --addr 127.0.0.1:7791        # 只查指定实例

示例：
  sudo vrndisd server --listen 0.0.0.0:7777
  sudo vrndisd client --server 1.2.3.4:7777
  sudo vrndisd client --server 1.2.3.4:7777 --reconnect --max-retries 0 --backoff 1s
  sudo vrndisd service install --mode server --listen 0.0.0.0:7777
  sudo vrndisd service install --mode server --listen 0.0.0.0:7777 --p2p --stun
  sudo vrndisd service install --mode client --server 1.2.3.4:7777 --p2p --reconnect
  vrndisd service install --print-config --mode client --server 1.2.3.4:7777
  vrndisd service export --output vrndisd.conf.bak
  vrndisd service status
`, Version)
}

// ---------- netcheck ----------

// cmdNetcheck 探测本机 NAT 环境与 UDP 可达性：同一 UDP socket 向主/第二
// STUN（及可选第三方 STUN）发送 Binding Request，比对各目的地映射。
func cmdNetcheck(args []string) error {
	fs := flag.NewFlagSet("netcheck", flag.ExitOnError)
	server := fs.String("server", "", "服务器 STUN 地址 host[:port]（默认端口 5478；留空=用 --config 或已安装配置连接的服务器）")
	extra := fs.String("extra-stun", "", "可选第三方 STUN host[:port]（跨公网 IP 映射比对）")
	cfgPath := fs.String("config", "", "配置文件路径（--server 未指定时从此读取服务器地址，适配 /etc 之外的非标准目录）")
	fs.Parse(args)
	var primary, secondary string
	if *server != "" {
		host, port, err := net.SplitHostPort(*server)
		if err != nil {
			host, port = *server, "5478"
		}
		if port == "" {
			port = "5478"
		}
		primary = net.JoinHostPort(host, port)
		secondary = net.JoinHostPort(host, "5479")
	} else {
		// 缺省用"连接的服务器"：读已安装配置（client 模式取其服务器，
		// server 模式回环本机）。STUN 端口固定 5478/5479——与客户端
		// candidateAddr 同一推导：STUN 走服务器 host + 固定端口，
		// 而非控制端口 7777。
		p, s, err := netcheckDefaultServer(*cfgPath)
		if err != nil {
			return err
		}
		primary, secondary = p, s
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fmt.Printf("netcheck：探测 %s / %s", primary, secondary)
	if *extra != "" {
		fmt.Printf(" / %s", *extra)
	}
	fmt.Println(" ...")
	res, err := netcheck.Run(ctx, primary, secondary, *extra)
	if err != nil {
		return err
	}
	fmt.Println("Report:")
	fmt.Printf("  UDP:                    %v\n", res.UDPOK)
	if res.Primary != nil {
		fmt.Printf("  映射(主 STUN):           %s (RTT %v)\n", res.Primary, fmtDur(res.PrimaryRTT))
	}
	if res.Secondary != nil {
		fmt.Printf("  映射(第二 STUN):         %s (RTT %v)\n", res.Secondary, fmtDur(res.SecondaryRTT))
	}
	if res.Extra != nil {
		fmt.Printf("  映射(第三方 STUN):       %s (RTT %v)\n", res.Extra, fmtDur(res.ExtraRTT))
	}
	fmt.Printf("  MappingVariesByDest:    %v\n", res.MappingVariesByDest)
	fmt.Printf("  NAT 类型:               %s\n", res.NATType)
	fmt.Printf("  打洞预期:               %s\n", res.HolepunchProspect)

	// 产品化结论：用户关心的是"能不能直连、否则走什么"，以及不确定时该做什么。
	fmt.Println("结论:")
	switch {
	case !res.UDPOK || res.NATType == "udp-blocked":
		fmt.Println("  预期路径: 服务器中继。本机 UDP 出站被阻断，节点间通信全部经服务器中继。")
	case res.UDPOK && res.Primary == nil:
		// 主 STUN 无应答但其他目的地有应答：典型为 --server 端口误填。
		fmt.Println("  预期路径: 暂按中继。主 STUN 探测无应答（端口可能误填）。")
		fmt.Println("  请检查: --server 端口应为 STUN 端口（默认 5478）；误填服务器")
		fmt.Println("         控制端口（TCP）会导致主探测无应答、NAT 判定降级。")
	case res.NATType == "unknown":
		fmt.Println("  预期路径: 暂按中继。仅单个目的地应答，无法比对映射。")
		fmt.Println("  可执行: 加 --extra-stun 指定第三方 STUN 再测，提高判定置信度。")
	case res.MappingVariesByDest:
		fmt.Println("  预期路径: 服务器中继。对称/多出口 NAT，直连成功率低，失败自动回退中继。")
	default:
		fmt.Println("  预期路径: P2P 优先。NAT 利于直连，无法直连时自动回退服务器中继。")
	}
	return nil
}

// netcheckDefaultServer 从已安装配置解析"连接的服务器"：
// client 模式取配置的服务器地址，server 模式回环本机；返回 STUN
// 主/第二探测地址（host + 5478/5479）。端口推导与客户端 candidateAddr
// 一致：STUN 挂在服务器公网 host 的固定端口，控制端口 7777 不作 STUN。
func netcheckDefaultServer(cfgPath string) (primary, secondary string, err error) {
	resolved, _, _, err := resolvePeerFlags("", "", "", cfgPath)
	if err != nil {
		return "", "", err
	}
	host, _, splitErr := net.SplitHostPort(resolved)
	if splitErr != nil {
		host = resolved
	}
	return net.JoinHostPort(host, "5478"), net.JoinHostPort(host, "5479"), nil
}

// isHelpToken 判断参数是否为请求帮助。
func isHelpToken(a string) bool {
	return a == "-h" || a == "--help"
}

// handleHelp 若参数含帮助请求，打印用法并返回 true。
func handleHelp(fs *flag.FlagSet, args []string) bool {
	for _, a := range args {
		if isHelpToken(a) {
			fs.SetOutput(os.Stdout)
			fs.Usage()
			return true
		}
	}
	return false
}

// parseLoose 宽松解析：允许位置参数出现在 flags 之前（如
// `vrndisd ping 10.77.0.4 --server x`）。逐条消费 flags，返回位置参数。
func parseLoose(fs *flag.FlagSet, args []string) []string {
	var positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if len(a) > 1 && a[0] == '-' {
			if strings.Contains(a, "=") {
				_ = fs.Parse([]string{a})
				continue
			}
			fl := fs.Lookup(strings.TrimLeft(a, "-"))
			if fl == nil { // 未注册 flag（如 --help）：当作位置参数，由调用方决定如何处理
				positional = append(positional, a)
				continue
			}
			if bf, ok := fl.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
				_ = fs.Parse([]string{a})
				continue
			}
			if i+1 < len(args) {
				_ = fs.Parse([]string{a, args[i+1]})
				i++
				continue
			}
		}
		positional = append(positional, a)
	}
	return positional
}

// fmtDur tailscale 风格的 RTT 格式化：<1ms 显示 µs。
func fmtDur(d time.Duration) string {
	if d < time.Millisecond {
		return fmt.Sprintf("%dµs", d.Microseconds())
	}
	return fmt.Sprintf("%.1fms", float64(d.Seconds())*1e3)
}

// ---------- ping / peers ----------

// discoProbe 向候选地址发送 disco 探测包并等待对端 tunnel 原路回包。
// 返回直连 RTT；超时/错误时返回非 nil error。
func discoProbe(timeout time.Duration, cand string) (time.Duration, error) {
	raddr, err := net.ResolveUDPAddr("udp", cand)
	if err != nil {
		return 0, err
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	start := time.Now()
	if _, err := conn.WriteToUDP(probeMagicBytes(), raddr); err != nil {
		return 0, err
	}
	_ = conn.SetReadDeadline(start.Add(timeout))
	buf := make([]byte, 64)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			return 0, err
		}
		if n == len(probeMagicBytes()) && string(buf[:n]) == string(probeMagicBytes()) {
			_ = from
			return time.Since(start), nil
		}
	}
}

// discoProbeMulti 并发探测全部候选，返回首个命中的 RTT 与候选地址。
// 多候选拨号是"先通后升"诊断的正确姿势：同 LAN 私网候选常可秒回，
// 而公网 srflx 在 CGNAT 无 hairpin 时永远不通——只测主候选（首个公网）
// 会把 LAN 直连误报成"直连无响应，走服务器中继"。
func discoProbeMulti(cands []string, wait time.Duration) (time.Duration, string) {
	if len(cands) == 0 {
		return 0, ""
	}
	type hit struct {
		rtt  time.Duration
		cand string
	}
	done := make(chan hit, len(cands))
	for _, c := range cands {
		go func(c string) {
			if rtt, err := discoProbe(wait, c); err == nil {
				done <- hit{rtt, c}
			} else {
				done <- hit{}
			}
		}(c)
	}
	deadline := time.NewTimer(wait + 300*time.Millisecond)
	defer deadline.Stop()
	for i := 0; i < len(cands); i++ {
		select {
		case h := <-done:
			if h.cand != "" {
				return h.rtt, h.cand
			}
		case <-deadline.C:
			return 0, ""
		}
	}
	return 0, ""
}

// loadInstalledConfig 从默认路径加载已安装服务配置（service install 写入）。
// 包级变量便于单测注入。
var loadInstalledConfig = func() (*service.Config, error) {
	return service.Load(service.DefaultConfigPath())
}

// resolvePeerFlags 为 ping/peers/netcheck 解析服务器参数：优先命令行 flag，
// 缺省回退到已安装服务配置（service install 写入 DefaultConfigPath），
// 实现"本机已配置客户端/服务器时免输 --server/--psk"。
// server 模式配置回退到回环地址直连本机服务器。
func resolvePeerFlags(server, psk, name, cfgPath string) (string, string, string, error) {
	if server != "" && cfgPath == "" {
		return server, psk, name, nil
	}
	var cfg *service.Config
	var loadErr error
	if cfgPath != "" {
		cfg, loadErr = service.Load(cfgPath)
		if loadErr != nil {
			return "", "", "", fmt.Errorf("--config %s 读取失败: %w", cfgPath, loadErr)
		}
	} else {
		cfg, loadErr = loadInstalledConfig()
	}
	if loadErr != nil {
		return "", "", "", fmt.Errorf(
			"--server 必填（未指定且无法读取已安装配置 %s: %v；"+
				"可用 vrndisd service install --mode client --server HOST:7777 预置，"+
				"或加 --config 指定其它路径的配置文件）",
			service.DefaultConfigPath(), loadErr)
	}
	switch cfg.Mode {
	case "client":
		if cfg.Server == "" {
			return "", "", "", errors.New("已安装配置为 client 模式但缺少 server 地址")
		}
	case "server":
		if cfg.Listen == "" {
			return "", "", "", errors.New("已安装配置为 server 模式但缺少 listen 地址")
		}
		cfg.Server = loopbackAddr(cfg.Listen)
	default:
		return "", "", "", fmt.Errorf("已安装配置 mode 无效: %q", cfg.Mode)
	}
	if server == "" {
		server = cfg.Server
	}
	if psk == "" {
		psk = cfg.PSK
	}
	if name == "" {
		name = cfg.Name
	}
	return server, psk, name, nil
}

// loopbackAddr 把监听地址中的通配 host（0.0.0.0/[::]/空）替换为 127.0.0.1，
// 供本机回环连接；其它 host 原样返回。
func loopbackAddr(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return net.JoinHostPort("127.0.0.1", port)
	}
	return listen
}

func probeMagicBytes() []byte {
	return []byte("vrndis-p2p-probe\x00\x01")
}

// cmdPing tailscale 风格的 peer 路径探测：控制信道连接服务器，
// PeerQuery 拿目标候选 → disco 探测直连路径；无响应则报中继基准。
func cmdPing(args []string) error {
	fs := flag.NewFlagSet("ping", flag.ExitOnError)
	server := fs.String("server", "", "服务器地址 host:port")
	psk := fs.String("psk", "", "预共享密钥（启用加密时必填）")
	cfgPath := fs.String("config", "", "配置文件路径（--server/psk/name 未指定时从此读取，适配 /etc 之外的非标准目录）")
	count := fs.Int("count", 5, "探测次数")
	wait := fs.Duration("wait", 3*time.Second, "每次探测等待超时")
	name := fs.String("name", "", "客户端名称（默认 ping，或取自已安装配置）")
	if handleHelp(fs, args) {
		return nil
	}
	positional := parseLoose(fs, args)
	target := ""
	if len(positional) > 0 {
		target = positional[0]
	}
	serverAddr, pskStr, nameStr, err := resolvePeerFlags(*server, *psk, *name, *cfgPath)
	if err != nil {
		return err
	}
	if nameStr == "" {
		nameStr = "ping"
	}
	if target == "" {
		return errors.New("用法: vrndisd ping <隧道IP或MAC>")
	}

	var pskBytes []byte
	if pskStr != "" {
		pskBytes = []byte(pskStr)
	}
	sess, _, err := ctrl.Dial(serverAddr, nameStr, pskBytes, 8*time.Second)
	if err != nil {
		return err
	}
	defer sess.Close()

	// 查目标候选
	body, _ := json.Marshal(proto.PeerQuery{Version: proto.ProtocolVersion, Target: target})
	if err := sess.Send(proto.TypePeerQuery, body); err != nil {
		return err
	}
	candMsg, err := sess.RecvDeadline(time.Now().Add(5 * time.Second))
	if err != nil {
		return fmt.Errorf("读取 PeerQuery 应答超时: %w", err)
	}
	if candMsg.Type != proto.TypePeerCandidate {
		return fmt.Errorf("期望 PeerCandidate，收到 %s", proto.TypeName(candMsg.Type))
	}
	var cand proto.PeerCandidate
	if err := json.Unmarshal(candMsg.Body, &cand); err != nil {
		return err
	}
	if cand.Candidate == "" {
		if cand.PeerMAC != "" {
			fmt.Printf("peer %s：在线，经服务器中继（未上报候选：未开 -p2p 或打洞失败已撤回）\n", target)
		} else {
			fmt.Printf("peer %s：不存在\n", target)
		}
		return nil
	}
	label := target
	if cand.PeerName != "" {
		label = fmt.Sprintf("%s (%s)", target, cand.PeerName)
	}

	// relay 基准 RTT
	var relayRTT time.Duration
	if r, err := sess.PingRTT(3 * time.Second); err == nil {
		relayRTT = r
	}

	// 探测列表：全部候选优先（含 LAN），旧服务器无 Candidates 时回退主候选
	probeList := cand.Candidates
	if len(probeList) == 0 {
		probeList = []string{cand.Candidate}
	}

	p2pHit := 0
	relayHit := 0
	for i := 0; i < *count; i++ {
		if rtt, hit := discoProbeMulti(probeList, *wait); hit != "" {
			p2pHit++
			fmt.Printf("pong from %s via p2p(%s) in %s\n", label, hit, fmtDur(rtt))
			continue
		}
		if relayRTT > 0 {
			relayHit++
			fmt.Printf("pong from %s via relay in %s（直连无响应，走服务器中继）\n", label, fmtDur(relayRTT))
		} else {
			fmt.Printf("pong from %s: 无响应\n", target)
		}
		time.Sleep(500 * time.Millisecond)
	}
	// 诊断提示：全程无直连回包时给一次结论性说明（原因 + 自助命令）
	if p2pHit == 0 && relayHit > 0 {
		fmt.Printf("提示: 本轮未收到直连回包（对端未上线 / 对称 NAT / 本 NAT 无 hairpin），")
		fmt.Printf("当前经服务器中继通信；netcheck 命令可评估本机打洞能力\n")
	}
	return nil
}

// cmdPeers 列出全部在线 peer 并并发 disco 探测路径状态。
func cmdPeers(args []string) error {
	fs := flag.NewFlagSet("peers", flag.ExitOnError)
	server := fs.String("server", "", "服务器地址 host:port")
	psk := fs.String("psk", "", "预共享密钥（启用加密时必填）")
	cfgPath := fs.String("config", "", "配置文件路径（--server/psk/name 未指定时从此读取，适配 /etc 之外的非标准目录）")
	wait := fs.Duration("wait", 3*time.Second, "每个 peer 探测超时")
	name := fs.String("name", "", "客户端名称（默认 peers，或取自已安装配置）")
	if handleHelp(fs, args) {
		return nil
	}
	parseLoose(fs, args)
	serverAddr, pskStr, nameStr, err := resolvePeerFlags(*server, *psk, *name, *cfgPath)
	if err != nil {
		return err
	}
	if nameStr == "" {
		nameStr = "peers"
	}

	var pskBytes []byte
	if pskStr != "" {
		pskBytes = []byte(pskStr)
	}
	sess, _, err := ctrl.Dial(serverAddr, nameStr, pskBytes, 8*time.Second)
	if err != nil {
		return err
	}
	defer sess.Close()

	body, _ := json.Marshal(proto.PeerQuery{Version: proto.ProtocolVersion})
	if err := sess.Send(proto.TypePeerQuery, body); err != nil {
		return err
	}
	// 收集应答直到超时
	var cands []proto.PeerCandidate
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		msg, err := sess.RecvDeadline(time.Now().Add(500 * time.Millisecond))
		if err != nil {
			break
		}
		if msg.Type != proto.TypePeerCandidate {
			continue
		}
		var c proto.PeerCandidate
		if err := json.Unmarshal(msg.Body, &c); err != nil {
			continue
		}
		if c.Candidate == "" && c.PeerMAC == "" {
			break // 全空应答：列表结束
		}
		cands = append(cands, c) // Candidate 为空但 PeerMAC 非空：在线 relay-only 设备
	}
	if len(cands) == 0 {
		fmt.Println("无在线 peer（或均未上报 P2P 候选）")
		return nil
	}

	var relayRTT time.Duration
	if r, err := sess.PingRTT(3 * time.Second); err == nil {
		relayRTT = r
	}
	// 以本机为基准：先问本机常驻 client 的真实隧道（对端 MAC -> remote 地址），
	// 服务器返回的候选是快照，嵌套 NAT 下常不可达，直接照着探会误报 relay。
	tunnels := localTunnels()

	fmt.Printf("在线 peer (%d)：\n", len(cands))
	var wg sync.WaitGroup
	results := make([]string, len(cands))
	for i, c := range cands {
		wg.Add(1)
		go func(i int, c proto.PeerCandidate) {
			defer wg.Done()
			label := c.PeerIP
			if c.PeerName != "" {
				label = fmt.Sprintf("%s (%s)", c.PeerIP, c.PeerName)
			}
			if c.Candidate == "" {
				// 在线但未上报候选（未开 -p2p / 打洞失败已撤回）：只能走中继。
				results[i] = fmt.Sprintf("  %-22s relay-only（未上报候选）", label)
				return
			}
			// 本机正好与此 peer 有活跃隧道：直接测隧道真实对端地址
			if tun, ok := tunnels[c.PeerMAC]; ok && tun.Remote != "" {
				if rtt, err := discoProbe(*wait, tun.Remote); err == nil {
					results[i] = fmt.Sprintf("  %-22s via p2p(%s)  %s（本机隧道）", label, tun.Remote, fmtDur(rtt))
					return
				}
				// 隧道存在但探不通（可能正切换中）：fallthrough 到候选探测
			}
			probeList := c.Candidates
			if len(probeList) == 0 {
				probeList = []string{c.Candidate}
			}
			if rtt, hit := discoProbeMulti(probeList, *wait); hit != "" {
				results[i] = fmt.Sprintf("  %-22s via p2p(%s)  %s", label, hit, fmtDur(rtt))
			} else if relayRTT > 0 {
				results[i] = fmt.Sprintf("  %-22s via relay  %s", label, fmtDur(relayRTT))
			} else {
				results[i] = fmt.Sprintf("  %-22s 无响应", label)
			}
		}(i, c)
	}
	wg.Wait()
	p2pHit := false
	for _, r := range results {
		if strings.Contains(r, "via p2p") {
			p2pHit = true
		}
		fmt.Println(r)
	}
	// 诊断提示：全部 peer 无直连回包时给一次结论性说明
	if !p2pHit && len(results) > 0 {
		fmt.Println("提示: 本轮无任何直连回包，全部经服务器中继；netcheck 命令可评估本机打洞能力")
	}
	return nil
}

// ---------- server ----------

// startStunServer 可选地同进程启动内置 STUN 服务器（NAT 穿透）。
// stun 为假时返回 (nil, nil, nil)；否则启动 STUN 服务器，返回：
//   - srv    服务器实例（供上层 Stop 优雅关停）
//   - errCh  启动失败时送入错误，供上层整体优雅关停（端口占用等）
//   - done   Start 返回后关闭，用于验证协程退出
func startStunServer(stun bool, listen, extra string) (*stunserver.Server, <-chan error, <-chan struct{}) {
	if !stun {
		return nil, nil, nil
	}
	srv := stunserver.New(stunserver.Config{ListenAddr: listen, ExtraAddr: extra})
	errCh := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		err := srv.Start()
		if err != nil {
			errCh <- err
		}
		close(done)
	}()
	return srv, errCh, done
}

func cmdServer(args []string) error {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	cfg := server.DefaultConfig()
	fs.StringVar(&cfg.Listen, "listen", cfg.Listen, "监听地址 host:port")
	fs.StringVar(&cfg.Network, "network", cfg.Network, "虚拟地址池 CIDR")
	fs.StringVar(&cfg.Gateway, "gateway", cfg.Gateway, "网关 / 服务器自身地址")
	fs.String("dns", strings.Join(cfg.DNS, ","), "下发 DNS，逗号分隔")
	fs.StringVar(&cfg.TapName, "tap", cfg.TapName, "服务器侧网卡名")
	fs.StringVar(&cfg.TapMAC, "tap-mac", cfg.TapMAC, "服务器网卡 MAC")
	fs.IntVar(&cfg.MTU, "mtu", cfg.MTU, "MTU")
	p2p := fs.Bool("p2p", false, "开启 P2P 直连（客户端间 NAT 穿透走 UDP，失败回退服务器中继）")
	stun := fs.Bool("stun", false, "同时启动内置 STUN 服务器（NAT 穿透）")
	stunListen := fs.String("stun-listen", ":5478", "STUN 监听地址（--stun 时生效）")
	stunListenExtra := fs.String("stun-listen-extra", ":5479", "STUN 第二监听地址（netcheck 跨目的地映射比对，检测 symmetric NAT）")
	psk := fs.String("psk", "", "预共享密钥（非空时要求客户端加密握手）")
	mgmtListen := fs.String("management-listen", "", "本机管理端口（默认 127.0.0.1:7790，- 禁用）")
	logLevel := fs.String("log-level", "", "日志级别 debug/info/warn/error（默认 info；debug 含逐帧事件，弱网会刷屏）")
	leaseFile := fs.String("lease-file", "", "按 name 持久化租约的文件路径（服务器重启后客户端复用原 IP/MAC；留空禁用）")
	fs.Parse(args)
	if *mgmtListen != "" {
		cfg.ManagementListen = *mgmtListen
	}
	if *leaseFile != "" {
		cfg.LeaseFile = *leaseFile
	}
	if *logLevel != "" {
		if _, err := loglevel.ParseLevel(*logLevel); err != nil {
			return err
		}
		cfg.LogLevel = *logLevel
	}
	cfg.DNS = splitCSV(fs.Lookup("dns").Value.String())
	if *psk != "" {
		cfg.PSK = []byte(*psk)
	}
	// P2P 必须在 server.New 前生效：此前赋值在 New 之后，服务器内部存的是
	// false 副本，CLI 模式的 --p2p 从未真正开启 P2P（配置文件路径没这问题）。
	cfg.P2P = *p2p

	srv, err := server.New(cfg)
	if err != nil {
		return err
	}
	if err := srv.Start(); err != nil {
		return err
	}
	log.Println("vrndisd: RNDIS 服务器已启动")

	stunSrv, stunErr, _ := startStunServer(*stun, *stunListen, *stunListenExtra)

	// 等待退出信号；若 STUN 启动失败（如端口占用），也整体优雅关停。
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case <-sig:
		signal.Stop(sig)
		log.Println("vrndisd: 收到退出信号，正在停止...")
	case err := <-stunErr:
		log.Printf("vrndisd: STUN 服务器启动失败，正在停止: %v", err)
	}

	srv.Stop()
	if stunSrv != nil {
		stunSrv.Stop()
	}
	log.Println("vrndisd: 服务器已停止")
	return nil
}

// ---------- client ----------

func cmdClient(args []string) error {
	fs := flag.NewFlagSet("client", flag.ExitOnError)
	var cfg client.Config
	reconnect := fs.Bool("reconnect", true, "断线自动重连（默认开启）")
	maxRetries := fs.Int("max-retries", 0, "最大重连次数（0=无限）")
	backoff := fs.Duration("backoff", time.Second, "重连退避基数")
	hbInterval := fs.Duration("heartbeat-interval", 0, "心跳发送间隔（0=默认 10s）")
	hbTimeout := fs.Duration("heartbeat-timeout", 0, "心跳超时阈值（0=默认 30s）")
	fs.StringVar(&cfg.Server, "server", "", "服务器地址 host:port（留空时回退已安装配置，同 netcheck/ping/peers）")
	fs.StringVar(&cfg.TapName, "tap", "vrndis-c0", "本地虚拟网卡名")
	fs.StringVar(&cfg.Name, "name", "", "客户端名称（默认主机名或取自已安装配置）")
	fs.BoolVar(&cfg.DefaultRoute, "default-route", false, "默认路由指向虚拟网卡")
	fs.BoolVar(&cfg.P2P, "p2p", false, "开启 P2P 直连（客户端间 NAT 穿透走 UDP，失败回退服务器中继）")
	fs.StringVar(&cfg.STUNServer, "stun-server", "", "STUN 服务器地址（P2P 时使用，默认用服务器地址）")
	psk := fs.String("psk", "", "预共享密钥（非空时与服务器加密握手）")
	fs.StringVar(&cfg.ManagementListen, "management-listen", "", "本机管理端口（默认 127.0.0.1:7790，同机多实例自动避让，- 禁用）")
	logLevel := fs.String("log-level", "", "日志级别 debug/info/warn/error（默认 info；debug 含逐帧事件，弱网会刷屏）")
	fs.Parse(args)
	if *logLevel != "" {
		if _, err := loglevel.ParseLevel(*logLevel); err != nil {
			return err
		}
		cfg.LogLevel = *logLevel
	}
	// --server 留空时回退已安装配置（与 netcheck/ping/peers 一致）：
	// 客户端模式取其 server 地址，服务器模式回环直连本机；psk/name 一并补齐。
	// 未安装配置时 resolvePeerFlags 会报错并给出预置方法。
	if cfg.Server == "" {
		srv, pskStr, nameStr, err := resolvePeerFlags("", *psk, cfg.Name, "")
		if err != nil {
			return err
		}
		cfg.Server, *psk, cfg.Name = srv, pskStr, nameStr
	}
	cfg.Reconnect = *reconnect
	cfg.MaxRetries = *maxRetries
	if *backoff > 0 {
		cfg.ReconnectBackoff = *backoff
	}
	cfg.HeartbeatInterval = *hbInterval
	cfg.HeartbeatTimeout = *hbTimeout
	if *psk != "" {
		cfg.PSK = []byte(*psk)
	}

	c := client.New(cfg)
	ctx, stopSig := signalContext()
	defer stopSig()

	if err := c.Run(ctx); err != nil {
		if ctx.Err() != nil {
			// 收到退出信号，属正常下线
			log.Println("vrndisd: 正在断开客户端...")
			return nil
		}
		return err
	}
	log.Println("vrndisd: 正在断开客户端...")
	return nil
}

// signalContext 返回一个在收到 SIGINT/SIGTERM 时取消的 context，供客户端自愈循环使用。
func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		signal.Stop(ch)
		cancel()
	}()
	return ctx, cancel
}

// ---------- service ----------

func cmdService(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("用法: vrndisd service install|uninstall|start|stop|status|export")
	}
	action := args[0]
	fs := flag.NewFlagSet("service", flag.ExitOnError)
	mode := fs.String("mode", "server", "运行模式: server|client（install/export 用）")
	cfgPath := fs.String("config", defaultConfigPath(), "配置文件路径")
	listen := fs.String("listen", "0.0.0.0:7777", "server: 监听地址 host:port")
	network := fs.String("network", "10.77.0.0/24", "server: 虚拟地址池 CIDR")
	gateway := fs.String("gateway", "10.77.0.1", "server: 网关")
	dns := fs.String("dns", "8.8.8.8,1.1.1.1", "server: 下发 DNS，逗号分隔")
	tapName := fs.String("tap", "", "网卡名（默认 server=vrndis0 client=vrndis-c0）")
	tapMAC := fs.String("tap-mac", "02:00:00:00:00:00", "server: 网卡 MAC")
	mtu := fs.Int("mtu", 1500, "server: MTU")
	stun := fs.Bool("stun", false, "server: 同时启动内置 STUN 服务器（NAT 穿透）")
	stunListen := fs.String("stun-listen", ":5478", "server: STUN 监听地址（--stun 时生效）")
	stunListenExtra := fs.String("stun-listen-extra", ":5479", "server: STUN 第二监听（netcheck 映射比对）")
	p2p := fs.Bool("p2p", false, "开启 P2P 直连（客户端间 NAT 穿透走 UDP，失败回退服务器中继）")
	serverAddr := fs.String("server", "", "client: 服务器地址 host:port")
	name := fs.String("name", "", "client: 客户端名称（默认主机名）")
	defRoute := fs.Bool("default-route", false, "client: 默认路由走隧道")
	reconnect := fs.Bool("reconnect", true, "client: 断线自动重连")
	maxRetries := fs.Int("max-retries", 0, "client: 最大重连次数（0=无限）")
	backoff := fs.Duration("backoff", time.Second, "client: 重连退避基数")
	hbInterval := fs.Duration("heartbeat-interval", 0, "client: 心跳发送间隔（0=默认 10s）")
	hbTimeout := fs.Duration("heartbeat-timeout", 0, "client: 心跳超时阈值（0=默认 30s）")
	stunServer := fs.String("stun-server", "", "client: STUN 服务器地址（P2P 时使用，默认用服务器地址）")
	psk := fs.String("psk", "", "预共享密钥（非空时启用加密握手，server/client 通用）")
	mgmtListen := fs.String("management-listen", "", "本机管理端口（默认 127.0.0.1:7790，同机多实例自动避让，- 禁用）")
	logLevel := fs.String("log-level", "", "日志级别 debug/info/warn/error（默认 info；debug 含逐帧事件，弱网会刷屏）")
	leaseFile := fs.String("lease-file", "", "server: 按 name 持久化租约的文件路径（重启后客户端复用原 IP/MAC；留空禁用）")
	output := fs.String("output", "", "export: 写入的目标文件（默认打印到 stdout）")
	printCfg := fs.Bool("print-config", false, "install: 仅打印将写入的配置 JSON，不安装服务")
	fs.Parse(args[1:])
	// 非法级别在此早报错（Visit 闭包无法 return）。
	if _, err := loglevel.ParseLevel(*logLevel); err != nil {
		return err
	}

	mgr := service.NewManager()
	switch action {
	case "install":
		// 以模式默认值为基底，仅用显式指定的 flag 覆盖：
		// 未传的 flag 保持该模式的默认值，避免 server/client 参数互相污染。
		cfg := service.DefaultConfig(*mode)
		fs.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "listen":
				cfg.Listen = *listen
			case "network":
				cfg.Network = *network
			case "gateway":
				cfg.Gateway = *gateway
			case "dns":
				cfg.DNS = splitCSV(*dns)
			case "tap":
				cfg.Tap = *tapName
			case "tap-mac":
				cfg.TapMAC = *tapMAC
			case "mtu":
				cfg.MTU = *mtu
			case "stun":
				cfg.STUN = *stun
			case "stun-listen":
				cfg.STUNListen = *stunListen
				cfg.STUNListenExtra = *stunListenExtra
			case "p2p":
				cfg.P2P = *p2p
			case "server":
				cfg.Server = *serverAddr
			case "name":
				cfg.Name = *name
			case "default-route":
				cfg.DefaultRoute = *defRoute
			case "reconnect":
				cfg.Reconnect = *reconnect
			case "max-retries":
				cfg.MaxRetries = *maxRetries
			case "backoff":
				cfg.ReconnectBackoff = service.Duration(*backoff)
			case "heartbeat-interval":
				cfg.HeartbeatInterval = service.Duration(*hbInterval)
			case "heartbeat-timeout":
				cfg.HeartbeatTimeout = service.Duration(*hbTimeout)
			case "stun-server":
				cfg.STUNServer = *stunServer
			case "psk":
				cfg.PSK = *psk
			case "management-listen":
				cfg.ManagementListen = *mgmtListen
			case "log-level":
				cfg.LogLevel = *logLevel
			case "lease-file":
				cfg.LeaseFile = *leaseFile
			}
		})
		if err := cfg.Validate(); err != nil {
			return err
		}
		if *printCfg {
			data, err := json.MarshalIndent(&cfg, "", "  ")
			if err != nil {
				return err
			}
			fmt.Println(string(data))
			return nil
		}
		if err := mgr.Install(&cfg, *cfgPath); err != nil {
			return err
		}
		log.Printf("vrndisd: 配置已写入 %s，服务 %s 已安装并启动", *cfgPath, service.ServiceName)
		return nil
	case "export":
		// 读取已安装服务的配置并序列化为 JSON；配置文件缺失时回退为模式默认值，便于作为模板写入。
		var cfg service.Config
		if _, err := os.Stat(*cfgPath); err == nil {
			loaded, err := service.Load(*cfgPath)
			if err != nil {
				return err
			}
			cfg = *loaded
		} else if os.IsNotExist(err) {
			cfg = service.DefaultConfig(*mode)
		} else {
			return err
		}
		if *output != "" {
			if err := cfg.Save(*output); err != nil {
				return err
			}
			log.Printf("vrndisd: 配置已导出到 %s", *output)
			return nil
		}
		data, err := json.MarshalIndent(&cfg, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	case "uninstall":
		if err := mgr.Uninstall(); err != nil {
			return err
		}
		log.Printf("vrndisd: 服务 %s 已卸载", service.ServiceName)
		return nil
	case "start":
		return mgr.Start()
	case "stop":
		return mgr.Stop()
	case "status":
		st, err := mgr.Status()
		if err != nil {
			return err
		}
		fmt.Printf("服务 %s: installed=%v running=%v (%s)\n",
			service.ServiceName, st.Installed, st.Running, st.Detail)
		return nil
	default:
		return fmt.Errorf("未知 service 子命令: %q", action)
	}
}

// ---------- service-run ----------

func cmdServiceRun(args []string, globalCfg string) error {
	fs := flag.NewFlagSet("service-run", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath(), "配置文件路径")
	// CLI 覆盖项（调试/临时改动用）：仅显式指定的参数覆盖配置文件内容。
	listen := fs.String("listen", "", "覆盖配置: 监听地址")
	network := fs.String("network", "", "覆盖配置: 虚拟地址池 CIDR")
	gateway := fs.String("gateway", "", "覆盖配置: 网关")
	tapName := fs.String("tap", "", "覆盖配置: 网卡名")
	serverAddr := fs.String("server", "", "覆盖配置: 服务器地址")
	p2p := fs.Bool("p2p", false, "覆盖配置: 开启 P2P 直连")
	stun := fs.Bool("stun", false, "覆盖配置: 启动内置 STUN 服务器")
	stunListen := fs.String("stun-listen", "", "覆盖配置: STUN 监听地址")
	stunListenExtra := fs.String("stun-listen-extra", "", "覆盖配置: STUN 第二监听地址")
	logLevel := fs.String("log-level", "", "覆盖配置: 日志级别 debug/info/warn/error")
	fs.Parse(args)
	if globalCfg != "" {
		*cfgPath = globalCfg
	}

	cfg, err := service.Load(*cfgPath)
	if err != nil {
		return err
	}
	// 覆盖项非法时直接中止，避免悄悄落到默认 info。
	if _, err := loglevel.ParseLevel(*logLevel); err != nil {
		return err
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "listen":
			cfg.Listen = *listen
		case "network":
			cfg.Network = *network
		case "gateway":
			cfg.Gateway = *gateway
		case "tap":
			cfg.Tap = *tapName
		case "server":
			cfg.Server = *serverAddr
		case "p2p":
			cfg.P2P = *p2p
		case "stun":
			cfg.STUN = *stun
		case "stun-listen":
			cfg.STUNListen = *stunListen
			cfg.STUNListenExtra = *stunListenExtra
		case "log-level":
			cfg.LogLevel = *logLevel
		}
	})

	return service.RunAsService(service.ServiceName, func(ctx context.Context) error {
		switch cfg.Mode {
		case "server":
			srv, err := server.New(server.Config{
				Listen:           cfg.Listen,
				Network:          cfg.Network,
				Gateway:          cfg.Gateway,
				DNS:              cfg.DNS,
				TapName:          cfg.Tap,
				TapMAC:           cfg.TapMAC,
				MTU:              cfg.MTU,
				P2P:              cfg.P2P,
				PSK:              []byte(cfg.PSK),
				ManagementListen: cfg.ManagementListen,
				LogLevel:         cfg.LogLevel,
				LeaseFile:        cfg.LeaseFile,
			})
			if err != nil {
				return err
			}
			if err := srv.Start(); err != nil {
				return err
			}
			log.Println("vrndisd: RNDIS 服务器已启动（服务模式）")

			// STUN 未开启时返回 nil channel，select 永不命中，无 goroutine 泄漏。
			stunSrv, stunErr, _ := startStunServer(cfg.STUN, cfg.STUNListen, cfg.STUNListenExtra)
			log.Println("vrndisd: 服务器已启动，等待退出信号...")
			select {
			case <-ctx.Done():
				log.Println("vrndisd: 服务收到停止信号")
			case err := <-stunErr:
				log.Printf("vrndisd: STUN 服务器启动失败，正在停止: %v", err)
			}
			srv.Stop()
			if stunSrv != nil {
				stunSrv.Stop()
			}
			log.Println("vrndisd: 服务器已停止")
			return nil
		case "client":
			c := client.New(client.Config{
				Server:            cfg.Server,
				TapName:           cfg.Tap,
				Name:              cfg.Name,
				DefaultRoute:      cfg.DefaultRoute,
				P2P:               cfg.P2P,
				STUNServer:        cfg.STUNServer,
				Reconnect:         cfg.Reconnect,
				MaxRetries:        cfg.MaxRetries,
				ReconnectBackoff:  time.Duration(cfg.ReconnectBackoff),
				HeartbeatInterval: time.Duration(cfg.HeartbeatInterval),
				HeartbeatTimeout:  time.Duration(cfg.HeartbeatTimeout),
				PSK:               []byte(cfg.PSK),
				ManagementListen:  cfg.ManagementListen,
				LogLevel:          cfg.LogLevel,
			})
			return c.Run(ctx)
		default:
			return fmt.Errorf("配置 mode 无效: %q", cfg.Mode)
		}
	})
}

// ---------- stun-server ----------

// cmdStunServer 内置 STUN 服务器子命令（原独立 cmd/stun-server 合并而来）。
func cmdStunServer(args []string) error {
	fs := flag.NewFlagSet("stun-server", flag.ExitOnError)
	listen := fs.String("listen", ":5478", "STUN 监听地址")
	showVersion := fs.Bool("version", false, "显示版本")
	fs.Parse(args)

	if *showVersion {
		fmt.Printf("vrndisd stun-server %s (%s/%s)\n", Version, runtime.GOOS, runtime.GOARCH)
		return nil
	}

	srv := stunserver.New(stunserver.Config{ListenAddr: *listen})
	if err := srv.Start(); err != nil {
		return err
	}
	waitSignal()
	log.Println("vrndisd: 正在停止 STUN 服务器...")
	srv.Stop()
	return nil
}

// ---------- 工具 ----------

// extractGlobalConfig 容忍 `--config X` 写在子命令之前（两种顺序都支持）。
func extractGlobalConfig(args []string) (rest []string, cfg string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--config" && i+1 < len(args):
			cfg = args[i+1]
			i++
		case strings.HasPrefix(a, "--config="):
			cfg = strings.TrimPrefix(a, "--config=")
		default:
			rest = append(rest, a)
		}
	}
	return rest, cfg
}

func waitSignal() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	<-ch
	signal.Stop(ch)
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// defaultConfigPath 委托给 service.DefaultConfigPath，平台路径保持单一事实来源。
func defaultConfigPath() string {
	return service.DefaultConfigPath()
}

// queryManagement 经本机管理端口查询服务器实时状态（进程内数据）。
// 不走客户端 TCP 协议、无需 PSK 握手。addr 为空时用默认 127.0.0.1:7790。
func queryManagement(addr, cmd string) (*proto.ManagementState, error) {
	if addr == "" {
		addr = defaultManagementAddr()
	}
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("连接管理端口失败 %s（服务器是否在运行？）: %v", addr, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	req := proto.ManagementRequest{Cmd: cmd}
	if err := json.NewEncoder(conn).Encode(&req); err != nil {
		return nil, fmt.Errorf("发送管理请求失败: %v", err)
	}
	var state proto.ManagementState
	if err := json.NewDecoder(conn).Decode(&state); err != nil {
		return nil, fmt.Errorf("读取响应失败: %v", err)
	}
	return &state, nil
}

// defaultManagementAddr 默认管理端口地址（与服务器端一致，仅回环）。
func defaultManagementAddr() string {
	return "127.0.0.1:7790"
}

// cmdStatus - 查询服务器状态和版本信息
func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	mgmtAddr := fs.String("addr", "", "管理端口地址（默认 127.0.0.1:7790）")
	fs.Parse(args)

	state, err := queryManagement(*mgmtAddr, "status")
	if err != nil {
		return err
	}
	fmt.Printf("状态: 版本=%s 在线客户端=%d\n", state.Version, len(state.Clients))
	return nil
}

// cmdClients - 查询在线客户端列表
func cmdClients(args []string) error {
	fs := flag.NewFlagSet("clients", flag.ExitOnError)
	mgmtAddr := fs.String("addr", "", "管理端口地址（默认 127.0.0.1:7790）")
	fs.Parse(args)

	state, err := queryManagement(*mgmtAddr, "clients")
	if err != nil {
		return err
	}
	fmt.Printf("在线客户端列表 (%d)：\n", len(state.Clients))
	if len(state.Clients) == 0 {
		fmt.Println("  （无在线客户端）")
	}
	for i, c := range state.Clients {
		fmt.Printf("  %d. %s (%s/%s) -> %s [p2p: %s]\n", i+1, c.IP, c.Name, c.MAC, c.Peer, c.P2P)
	}
	return nil
}

// localScanPorts 本机管理端口扫描范围（与 mgmt 包默认避让区间一致）。
const localScanPorts = 10

// cmdLocal 列出本机全部 vrndisd 实例：并发扫描本机管理端口 7790..7799，
// server 与每个 client 实例各打印一行概要。--addr 只查指定实例。
func cmdLocal(args []string) error {
	fs := flag.NewFlagSet("local", flag.ExitOnError)
	mgmtAddr := fs.String("addr", "", "只查指定管理端口地址（默认扫描 127.0.0.1:7790..7799）")
	fs.Parse(args)

	if *mgmtAddr != "" {
		state, err := queryManagement(*mgmtAddr, "status")
		if err != nil {
			return err
		}
		printInstance(*mgmtAddr, state)
		return nil
	}

	found := scanLocalStates()
	for _, r := range found {
		printInstance(fmt.Sprintf("127.0.0.1:%d", r.Port), &r.State)
	}
	if len(found) == 0 {
		fmt.Println("本机没有运行中的 vrndisd 实例（扫描 127.0.0.1:7790..7799 无响应）")
		fmt.Println("提示: client 的管理面随客户端一起运行；若用 --management-listen 改过端口，请用 --addr 指定")
	}
	return nil
}

// localInstance 本机管理端口扫描结果。
type localInstance struct {
	Port  int
	State proto.ManagementState
}

// scanLocalStates 并发扫描本机管理端口 7790..7799，按端口号升序返回全部实例状态。
// 端口无实例（连接拒绝）的项被跳过。
func scanLocalStates() []localInstance {
	type result struct {
		port  int
		state *proto.ManagementState
		err   error
	}
	results := make(chan result, localScanPorts)
	for i := 0; i < localScanPorts; i++ {
		port := 7790 + i
		go func() {
			state, err := queryManagement(fmt.Sprintf("127.0.0.1:%d", port), "status")
			results <- result{port, state, err}
		}()
	}
	var got []localInstance
	for i := 0; i < localScanPorts; i++ {
		r := <-results
		if r.err == nil && r.state != nil {
			got = append(got, localInstance{Port: r.port, State: *r.state})
		}
	}
	sort.Slice(got, func(i, j int) bool { return got[i].Port < got[j].Port })
	return got
}

// localTunnels 取本机所有 client 实例当前活跃的 P2P 隧道：对端虚拟 MAC -> 隧道状态。
// peers 等"以本机为基准"的诊断用它判断本机与某 peer 的真实数据面路径，
// 不再依赖服务器侧快照候选（嵌套 NAT 下候选常不可达，显示失真）。
func localTunnels() map[string]proto.ClientP2PStatus {
	tunnels := map[string]proto.ClientP2PStatus{}
	for _, inst := range scanLocalStates() {
		c := inst.State.Client
		if inst.State.Role != "client" || c == nil || !c.P2P.Active {
			continue
		}
		if c.P2P.PeerMAC != "" {
			tunnels[c.P2P.PeerMAC] = c.P2P
		}
		// 多对端隧道：路由表内每个已直连 peer 都按本机隧道处理，
		// 探测改用该路由的真实 UDP 端点（PeerMAC/Remote 覆盖主隧道值）。
		for mac, addr := range c.P2P.Routes {
			if _, ok := tunnels[mac]; ok {
				continue
			}
			st := c.P2P
			st.PeerMAC = mac
			st.Remote = addr
			tunnels[mac] = st
		}
	}
	return tunnels
}

// printInstance 打印单个本机实例状态概要（server / client 各一行，client 附 P2P 详情）。
func printInstance(addr string, state *proto.ManagementState) {
	if state.Role == "client" && state.Client != nil {
		c := state.Client
		status := "未连接"
		if c.Connected {
			status = "已连接 " + c.VirtualIP
			if c.VirtualMAC != "" {
				status += " (" + c.VirtualMAC + ")"
			}
		}
		p2p := "未启用"
		if c.P2P.Enabled {
			switch {
			case c.P2P.Active:
				p2p = "直连中 -> " + c.P2P.PeerMAC
				if c.P2P.Remote != "" {
					p2p += " @ " + c.P2P.Remote
				}
				if n := len(c.P2P.Routes); n > 1 {
					p2p += fmt.Sprintf("（共 %d 条直连路由）", n)
				}
				var ago []string
				if c.P2P.LastRxAgo != "" {
					ago = append(ago, "rx "+c.P2P.LastRxAgo)
				}
				if c.P2P.LastTxAgo != "" {
					ago = append(ago, "tx "+c.P2P.LastTxAgo)
				}
				if len(ago) > 0 {
					p2p += "（" + strings.Join(ago, " ") + "）"
				}
			case c.P2P.LastFail != "":
				p2p = "中继（" + c.P2P.LastFail + "）"
			default:
				p2p = "中继"
			}
		}
		name := c.Name
		if name == "" {
			name = "(未命名)"
		}
		fmt.Printf("  %s  client %s  %s\n", addr, state.Version, name)
		fmt.Printf("    server=%s %s，建连 %d 次，p2p: %s\n", c.Server, status, c.Connects, p2p)
		return
	}
	// 旧版服务器无 Role 字段，按 server 处理
	fmt.Printf("  %s  server %s  在线客户端=%d\n", addr, state.Version, len(state.Clients))
}
