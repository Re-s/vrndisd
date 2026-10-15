// vrndisd —— 基于 IP(TCP) 连接的虚拟 RNDIS 隧道（服务器/客户端/服务管理）。
//
// 用法：
//
//	vrndisd server  [--listen 0.0.0.0:7777] [--network 10.77.0.0/24] [--gateway 10.77.0.1] [--stun] [--stun-listen :5478] ...
//	vrndisd client  --server HOST:7777 [--tap vrndis-c0] [--name X] [--default-route]
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
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"vrndis/internal/client"
	"vrndis/internal/ctrl"
	"vrndis/internal/netcheck"
	"vrndis/internal/proto"
	"vrndis/internal/server"
	"vrndis/internal/service"
	stunserver "vrndis/internal/stun/server"
)

var Version = "1.0.1"

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
  version        显示版本号
  help           显示本帮助

示例（STUN）：
  sudo vrndisd server --listen 0.0.0.0:7777 --stun
  sudo vrndisd server --listen 0.0.0.0:7777 --stun --stun-listen 0.0.0.0:5478
  sudo vrndisd stun-server --listen 0.0.0.0:5478
  vrndisd stun-server --version

示例（netcheck）：
  vrndisd netcheck --server 1.2.3.4                      # 服务器双端口探测
  vrndisd netcheck --server 1.2.3.4 --extra-stun stun.l.google.com:19302

示例（ping / peers）：
  vrndisd ping 10.77.0.4                      # 免输：自动读取已安装配置的服务器/PSK
  vrndisd ping 10.77.0.4 --server 1.2.3.4:7777 --psk yourpsk
  vrndisd peers                              # 同上，列出全部在线 peer
  vrndisd peers --server 1.2.3.4:7777 --psk yourpsk

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
	server := fs.String("server", "", "服务器地址 host[:port]（默认端口 5478）")
	extra := fs.String("extra-stun", "", "可选第三方 STUN host[:port]（跨公网 IP 映射比对）")
	fs.Parse(args)
	if *server == "" {
		return errors.New("--server 必填（服务器 STUN 主地址）")
	}
	host, port, err := net.SplitHostPort(*server)
	if err != nil {
		host, port = *server, "5478"
	}
	if port == "" {
		port = "5478"
	}
	primary := net.JoinHostPort(host, port)
	secondary := net.JoinHostPort(host, "5479")

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
	return nil
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

// loadInstalledConfig 从默认路径加载已安装服务配置（service install 写入）。
// 包级变量便于单测注入。
var loadInstalledConfig = func() (*service.Config, error) {
	return service.Load(service.DefaultConfigPath())
}

// resolvePeerFlags 为 ping/peers 解析服务器参数：优先命令行 flag，
// 缺省回退到已安装服务配置（service install 写入 DefaultConfigPath），
// 实现"本机已配置客户端/服务器时免输 --server/--psk"。
// server 模式配置回退到回环地址直连本机服务器。
func resolvePeerFlags(server, psk, name string) (string, string, string, error) {
	if server != "" {
		return server, psk, name, nil
	}
	cfg, err := loadInstalledConfig()
	if err != nil {
		return "", "", "", fmt.Errorf(
			"--server 必填（未指定且无法读取已安装配置 %s: %v；"+
				"可用 vrndisd service install --mode client --server HOST:7777 预置）",
			service.DefaultConfigPath(), err)
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
	if psk == "" {
		psk = cfg.PSK
	}
	if name == "" {
		name = cfg.Name
	}
	return cfg.Server, psk, name, nil
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
	count := fs.Int("count", 5, "探测次数")
	wait := fs.Duration("wait", 3*time.Second, "每次探测等待超时")
	name := fs.String("name", "", "客户端名称（默认 ping，或取自已安装配置）")
	positional := parseLoose(fs, args)
	target := ""
	if len(positional) > 0 {
		target = positional[0]
	}
	serverAddr, pskStr, nameStr, err := resolvePeerFlags(*server, *psk, *name)
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
		fmt.Printf("peer %s：不存在或未上报候选（未开 -p2p / 打洞未活跃）\n", target)
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

	for i := 0; i < *count; i++ {
		if rtt, err := discoProbe(*wait, cand.Candidate); err == nil {
			fmt.Printf("pong from %s via p2p(%s) in %s\n", label, cand.Candidate, fmtDur(rtt))
			continue
		}
		if relayRTT > 0 {
			fmt.Printf("pong from %s via relay in %s（直连无响应，走服务器中继）\n", label, fmtDur(relayRTT))
		} else {
			fmt.Printf("pong from %s: 无响应\n", target)
		}
		time.Sleep(500 * time.Millisecond)
	}
	return nil
}

// cmdPeers 列出全部在线 peer 并并发 disco 探测路径状态。
func cmdPeers(args []string) error {
	fs := flag.NewFlagSet("peers", flag.ExitOnError)
	server := fs.String("server", "", "服务器地址 host:port")
	psk := fs.String("psk", "", "预共享密钥（启用加密时必填）")
	wait := fs.Duration("wait", 3*time.Second, "每个 peer 探测超时")
	name := fs.String("name", "", "客户端名称（默认 peers，或取自已安装配置）")
	parseLoose(fs, args)
	serverAddr, pskStr, nameStr, err := resolvePeerFlags(*server, *psk, *name)
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
		if c.Candidate == "" {
			break // 空应答：列表结束/无 peer
		}
		cands = append(cands, c)
	}
	if len(cands) == 0 {
		fmt.Println("无在线 peer（或均未上报 P2P 候选）")
		return nil
	}

	var relayRTT time.Duration
	if r, err := sess.PingRTT(3 * time.Second); err == nil {
		relayRTT = r
	}

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
			if rtt, err := discoProbe(*wait, c.Candidate); err == nil {
				results[i] = fmt.Sprintf("  %-22s via p2p(%s)  %s", label, c.Candidate, fmtDur(rtt))
			} else if relayRTT > 0 {
				results[i] = fmt.Sprintf("  %-22s via relay  %s", label, fmtDur(relayRTT))
			} else {
				results[i] = fmt.Sprintf("  %-22s 无响应", label)
			}
		}(i, c)
	}
	wg.Wait()
	for _, r := range results {
		fmt.Println(r)
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
	mgmtSock := fs.String("management-socket", "", "本机管理套接字路径（默认 /run/vrndisd/management.sock，- 禁用）")
	fs.Parse(args)
	if *mgmtSock != "" {
		cfg.ManagementSocket = *mgmtSock
	}
	cfg.DNS = splitCSV(fs.Lookup("dns").Value.String())
	if *psk != "" {
		cfg.PSK = []byte(*psk)
	}

	srv, err := server.New(cfg)
	if err != nil {
		return err
	}
	if err := srv.Start(); err != nil {
		return err
	}
	log.Println("vrndisd: RNDIS 服务器已启动")

	cfg.P2P = *p2p
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
	fs.StringVar(&cfg.Server, "server", "", "服务器地址 host:port（必填）")
	fs.StringVar(&cfg.TapName, "tap", "vrndis-c0", "本地虚拟网卡名")
	fs.StringVar(&cfg.Name, "name", "", "客户端名称（默认主机名）")
	fs.BoolVar(&cfg.DefaultRoute, "default-route", false, "默认路由指向虚拟网卡")
	fs.BoolVar(&cfg.P2P, "p2p", false, "开启 P2P 直连（客户端间 NAT 穿透走 UDP，失败回退服务器中继）")
	fs.StringVar(&cfg.STUNServer, "stun-server", "", "STUN 服务器地址（P2P 时使用，默认用服务器地址）")
	psk := fs.String("psk", "", "预共享密钥（非空时与服务器加密握手）")
	fs.Parse(args)
	if cfg.Server == "" {
		return fmt.Errorf("缺少 --server")
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
	mgmtSock := fs.String("management-socket", "", "server: 本机管理套接字路径（默认平台默认，- 禁用）")
	output := fs.String("output", "", "export: 写入的目标文件（默认打印到 stdout）")
	printCfg := fs.Bool("print-config", false, "install: 仅打印将写入的配置 JSON，不安装服务")
	fs.Parse(args[1:])

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
			case "management-socket":
				cfg.ManagementSocket = *mgmtSock
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
	fs.Parse(args)
	if globalCfg != "" {
		*cfgPath = globalCfg
	}

	cfg, err := service.Load(*cfgPath)
	if err != nil {
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
		}
	})

	return service.RunAsService(service.ServiceName, func(ctx context.Context) error {
		switch cfg.Mode {
		case "server":
			srv, err := server.New(server.Config{
				Listen:  cfg.Listen,
				Network: cfg.Network,
				Gateway: cfg.Gateway,
				DNS:     cfg.DNS,
				TapName: cfg.Tap,
				TapMAC:  cfg.TapMAC,
				MTU:     cfg.MTU,
				P2P:     cfg.P2P,
				PSK:     []byte(cfg.PSK),
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

// queryManagement 经本机管理套接字查询服务器实时状态（进程内数据）。
// 不走客户端 TCP 协议、无需 PSK 握手。path 为空时用平台默认套接字路径。
func queryManagement(path, cmd string) (*proto.ManagementState, error) {
	if path == "" {
		path = defaultManagementSocketPath()
	}
	conn, err := net.DialTimeout("unix", path, 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("连接管理套接字失败 %s（服务器是否在运行？）: %v", path, err)
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

// defaultManagementSocketPath 平台默认管理套接字路径（与服务器端一致）。
func defaultManagementSocketPath() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(os.Getenv("ProgramData"), "vrndisd", "management.sock")
	}
	return "/run/vrndisd/management.sock"
}

// cmdStatus - 查询服务器状态和版本信息
func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	socketPath := fs.String("socket", "", "管理套接字路径（默认服务器平台默认路径）")
	fs.Parse(args)

	state, err := queryManagement(*socketPath, "status")
	if err != nil {
		return err
	}
	fmt.Printf("状态: 版本=%s 在线客户端=%d\n", state.Version, len(state.Clients))
	return nil
}

// cmdClients - 查询在线客户端列表
func cmdClients(args []string) error {
	fs := flag.NewFlagSet("clients", flag.ExitOnError)
	socketPath := fs.String("socket", "", "管理套接字路径（默认服务器平台默认路径）")
	fs.Parse(args)

	state, err := queryManagement(*socketPath, "clients")
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
