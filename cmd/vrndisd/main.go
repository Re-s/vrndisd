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
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"vrndis/internal/client"
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
  status         服务器运行状态与在线客户端概况
  clients        列出当前在线客户端（名称/IP/MAC/对端）
  version        显示版本号
  help           显示本帮助

示例（STUN）：
  sudo vrndisd server --listen 0.0.0.0:7777 --stun
  sudo vrndisd server --listen 0.0.0.0:7777 --stun --stun-listen 0.0.0.0:5478
  sudo vrndisd stun-server --listen 0.0.0.0:5478
  vrndisd stun-server --version

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

// ---------- server ----------

// startStunServer 可选地同进程启动内置 STUN 服务器（NAT 穿透）。
// stun 为假时返回 (nil, nil, nil)；否则启动 STUN 服务器，返回：
//   - srv    服务器实例（供上层 Stop 优雅关停）
//   - errCh  启动失败时送入错误，供上层整体优雅关停（端口占用等）
//   - done   Start 返回后关闭，用于验证协程退出
func startStunServer(stun bool, listen string) (*stunserver.Server, <-chan error, <-chan struct{}) {
	if !stun {
		return nil, nil, nil
	}
	srv := stunserver.New(stunserver.Config{ListenAddr: listen})
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
	fs.Parse(args)
	cfg.DNS = splitCSV(fs.Lookup("dns").Value.String())

	srv, err := server.New(cfg)
	if err != nil {
		return err
	}
	if err := srv.Start(); err != nil {
		return err
	}
	log.Println("vrndisd: RNDIS 服务器已启动")

	cfg.P2P = *p2p
	stunSrv, stunErr, _ := startStunServer(*stun, *stunListen)

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
			})
			if err != nil {
				return err
			}
			if err := srv.Start(); err != nil {
				return err
			}
			log.Println("vrndisd: RNDIS 服务器已启动（服务模式）")

			// STUN 未开启时返回 nil channel，select 永不命中，无 goroutine 泄漏。
			stunSrv, stunErr, _ := startStunServer(cfg.STUN, cfg.STUNListen)
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

// sendManagementRequest 建立管理连接并发送管理请求，返回管理响应。
// 服务器在连接建立时会先给客户端下发 TypeAssign 握手（假定所有连接都是普通 RNDIS 客户端），
// 因此这里先发 Hello 握手，随后读取并跳过 Assign/心跳等非管理消息，直到拿到 TypeManagement 响应。
// 修复：之前直接期望第一个响应就是 TypeManagement，会撞上服务器先发的 Assign 而报“收到非管理消息类型”。
func sendManagementRequest(serverAddr, cmd string) (*proto.ManagementResponse, error) {
	conn, err := net.DialTimeout("tcp", serverAddr, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("连接服务器失败: %v", err)
	}
	defer conn.Close()

	// 模拟客户端握手：先发 HELLO（服务器兼容空/简单 body，仅用于登记名称）。
	if err := proto.WriteMessage(conn, proto.TypeHello, []byte(`{"name":"vrndisd-cli"}`)); err != nil {
		return nil, fmt.Errorf("发送握手失败: %v", err)
	}

	req := proto.ManagementRequest{Cmd: cmd}
	reqBytes, _ := json.Marshal(req)
	if err := proto.WriteMessage(conn, proto.TypeManagement, reqBytes); err != nil {
		return nil, fmt.Errorf("发送管理请求失败: %v", err)
	}

	// 服务器先发 Assign 握手，后回 Management 响应；跳过其间任何非管理消息。
	for i := 0; i < 8; i++ {
		msg, err := proto.ReadMessage(conn)
		if err != nil {
			return nil, fmt.Errorf("读取响应失败: %v", err)
		}
		if msg.Type == proto.TypeManagement {
			var resp proto.ManagementResponse
			if err := json.Unmarshal(msg.Body, &resp); err != nil {
				return nil, fmt.Errorf("解析响应失败: %v", err)
			}
			return &resp, nil
		}
		// 其它类型（Assign/Hello 应答/Pong 等）为握手与心跳，跳过继续等待管理响应。
	}
	return nil, fmt.Errorf("未收到管理响应")
}

// cmdStatus - 查询服务器状态和版本信息
func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	serverAddr := fs.String("server", "127.0.0.1:7777", "服务器地址")
	fs.Parse(args)

	resp, err := sendManagementRequest(*serverAddr, "status")
	if err != nil {
		return err
	}
	fmt.Printf("状态: 版本=%s 在线客户端=%d\n", resp.Version, resp.Clients)
	return nil
}

// cmdClients - 查询在线客户端列表
func cmdClients(args []string) error {
	fs := flag.NewFlagSet("clients", flag.ExitOnError)
	serverAddr := fs.String("server", "127.0.0.1:7777", "服务器地址")
	fs.Parse(args)

	resp, err := sendManagementRequest(*serverAddr, "clients")
	if err != nil {
		return err
	}
	fmt.Printf("在线客户端列表 (%d):\n", resp.Clients)
	for i, c := range resp.List {
		fmt.Printf("  %d. %s (%s) - %s\n", i+1, c.Name, c.MAC, c.Peer)
	}
	return nil
}
