// vrndisd —— 基于 IP(TCP) 连接的虚拟 RNDIS 隧道（服务器/客户端/服务管理）。
//
// 用法：
//
//	vrndisd server  [--listen 0.0.0.0:7777] [--network 10.77.0.0/24] [--gateway 10.77.0.1] ...
//	vrndisd client  --server HOST:7777 [--tap vrndis-c0] [--name X] [--default-route]
//	vrndisd service install|uninstall|start|stop|status [--mode server|client ...]
//	vrndisd service-run [--config PATH]
//	vrndisd version
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"vrndis/internal/client"
	"vrndis/internal/server"
	"vrndis/internal/service"
)

const version = "1.0.0"

func main() {
	log.SetFlags(log.LstdFlags)
	args, globalCfg := extractGlobalConfig(os.Args[1:])
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	var err error
	switch args[0] {
	case "server":
		err = cmdServer(args[1:])
	case "client":
		err = cmdClient(args[1:])
	case "service":
		err = cmdService(args[1:])
	case "service-run":
		err = cmdServiceRun(args[1:], globalCfg)
	case "version", "-V", "--version":
		fmt.Printf("vrndisd %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "[vrndisd] 错误: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`vrndisd - 基于 IP(TCP) 连接的虚拟 RNDIS 隧道

命令：
  server         启动服务器（分配地址 + 帧转发 + 网关）
  client         连接服务器，获得分配地址并建立本地虚拟网卡
  service        持久化服务管理：install|uninstall|start|stop|status
  service-run    服务入口（由 systemd/SCM 调用，也可前台运行）
  version        版本信息

示例：
  sudo vrndisd server --listen 0.0.0.0:7777
  sudo vrndisd client --server 1.2.3.4:7777
  sudo vrndisd service install --mode server --listen 0.0.0.0:7777
  sudo vrndisd service install --mode client --server 1.2.3.4:7777 --default-route
  vrndisd service status
`)
}

// ---------- server ----------

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
	fs.Parse(args)
	cfg.DNS = splitCSV(fs.Lookup("dns").Value.String())

	srv, err := server.New(cfg)
	if err != nil {
		return err
	}
	if err := srv.Start(); err != nil {
		return err
	}
	waitSignal()
	log.Println("vrndisd: 正在停止服务器...")
	srv.Stop()
	return nil
}

// ---------- client ----------

func cmdClient(args []string) error {
	fs := flag.NewFlagSet("client", flag.ExitOnError)
	var cfg client.Config
	fs.StringVar(&cfg.Server, "server", "", "服务器地址 host:port（必填）")
	fs.StringVar(&cfg.TapName, "tap", "vrndis-c0", "本地虚拟网卡名")
	fs.StringVar(&cfg.Name, "name", "", "客户端名称（默认主机名）")
	fs.BoolVar(&cfg.DefaultRoute, "default-route", false, "默认路由指向虚拟网卡")
	fs.Parse(args)
	if cfg.Server == "" {
		return fmt.Errorf("缺少 --server")
	}

	c := client.New(cfg)
	if _, err := c.Connect(); err != nil {
		return err
	}
	waitSignal()
	log.Println("vrndisd: 正在断开客户端...")
	c.Stop()
	return nil
}

// ---------- service ----------

func cmdService(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("用法: vrndisd service install|uninstall|start|stop|status")
	}
	action := args[0]
	fs := flag.NewFlagSet("service", flag.ExitOnError)
	mode := fs.String("mode", "server", "运行模式: server|client（install 用）")
	cfgPath := fs.String("config", defaultConfigPath(), "配置文件路径")
	listen := fs.String("listen", "0.0.0.0:7777", "server: 监听地址")
	network := fs.String("network", "10.77.0.0/24", "server: 虚拟地址池")
	gateway := fs.String("gateway", "10.77.0.1", "server: 网关")
	dns := fs.String("dns", "8.8.8.8,1.1.1.1", "server: 下发 DNS")
	tapName := fs.String("tap", "", "网卡名（默认 server=vrndis0 client=vrndis-c0）")
	tapMAC := fs.String("tap-mac", "02:00:00:00:00:00", "server: 网卡 MAC")
	mtu := fs.Int("mtu", 1500, "server: MTU")
	serverAddr := fs.String("server", "", "client: 服务器地址 host:port")
	name := fs.String("name", "", "client: 客户端名称")
	defRoute := fs.Bool("default-route", false, "client: 默认路由走隧道")
	fs.Parse(args[1:])

	mgr := service.NewManager()
	switch action {
	case "install":
		cfg := service.DefaultConfig(*mode)
		cfg.Listen, cfg.Network, cfg.Gateway = *listen, *network, *gateway
		cfg.DNS = splitCSV(*dns)
		cfg.TapMAC, cfg.MTU = *tapMAC, *mtu
		cfg.Server, cfg.Name, cfg.DefaultRoute = *serverAddr, *name, *defRoute
		if *tapName != "" {
			cfg.Tap = *tapName
		}
		if err := cfg.Validate(); err != nil {
			return err
		}
		if err := cfg.Save(*cfgPath); err != nil {
			return err
		}
		log.Printf("vrndisd: 配置已写入 %s", *cfgPath)
		if err := mgr.Install(*cfgPath); err != nil {
			return err
		}
		log.Printf("vrndisd: 服务 %s 已安装并启动", service.ServiceName)
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
	fs.Parse(args)
	if globalCfg != "" {
		*cfgPath = globalCfg
	}

	cfg, err := service.Load(*cfgPath)
	if err != nil {
		return err
	}
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
			})
			if err != nil {
				return err
			}
			if err := srv.Start(); err != nil {
				return err
			}
			<-ctx.Done()
			log.Println("vrndisd: 服务收到停止信号")
			srv.Stop()
			return nil
		case "client":
			c := client.New(client.Config{
				Server:       cfg.Server,
				TapName:      cfg.Tap,
				Name:         cfg.Name,
				DefaultRoute: cfg.DefaultRoute,
			})
			return c.Run(ctx)
		default:
			return fmt.Errorf("配置 mode 无效: %q", cfg.Mode)
		}
	})
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

func defaultConfigPath() string {
	if base := os.Getenv("ProgramData"); base != "" && runtime.GOOS == "windows" {
		return filepath.Join(base, "vrndisd", "vrndisd.conf")
	}
	if runtime.GOOS == "windows" {
		return filepath.Join(".", "vrndisd.conf")
	}
	return "/etc/vrndisd.conf"
}
