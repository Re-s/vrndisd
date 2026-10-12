# vrndis-go —— 虚拟 RNDIS 隧道（Go 实现）

Go 重写版：**单文件静态二进制**，性能更好、可交叉编译 Windows，内置**持久化服务配置工具**。

```
客户端 --TCP(IP)--> 服务器：分配 IP/网关/DNS/MAC --> 本机出现虚拟网卡 --> 以太网帧透明传输
```

## 特性

- **单二进制、零第三方依赖**（纯标准库，含 Windows SCM 接入）
- **地址即服务**：TCP 连上就拿到服务器分配的地址（10.77.0.0/24 池，默认网关 .1）
- **二层透传**：学习型交换机，客户端互通、ARP/ICMP 全程正常
- **健壮性设计**：
  - 会话与 TAP 写入均为**有界队列（512 帧）**，慢消费者/慢内核只丢自己的帧，绝不拖死全网
  - 畸形帧、垃圾消息、未知类型、超长声明全部容错（协议层 fuzz 测试覆盖）
  - 断线即回收租约，地址优先复用；协程/资源零泄漏（有回归测试）
- **服务化**：`vrndisd service install` 一条命令装成 systemd / Windows 服务（开机自启、崩溃自拉）
- **Linux/Windows**：数据面 Linux TAP（/dev/net/tun）/ Windows wintun；服务工具双平台

## 快速开始

### 前台运行

```bash
# 服务器（Linux，需 root）
sudo ./vrndisd server --listen 0.0.0.0:7777 --network 10.77.0.0/24 --gateway 10.77.0.1

# 客户端
sudo ./vrndisd client --server 服务器IP:7777
# → [vrndisd] 已连接 ...，本机地址 10.77.0.2/24 网关 10.77.0.1

# 加 --default-route 则全部流量走隧道（VPN 模式）
```

### 持久化服务（开机自启 + 崩溃自拉）

```bash
# Linux（systemd）
sudo ./vrndisd service install --mode server --listen 0.0.0.0:7777
sudo ./vrndisd service status          # 服务 vrndisd: installed=true running=true
sudo ./vrndisd service stop
sudo ./vrndisd service start
sudo ./vrndisd service uninstall

# 客户端模式服务
sudo ./vrndisd service install --mode client --server 1.2.3.4:7777 --default-route

# Windows（管理员 CMD/PowerShell，wintun.dll 放在 exe 同目录）
vrndisd.exe service install --mode server --listen 0.0.0.0:7777
vrndisd.exe service status
vrndisd.exe service uninstall
```

配置写入 `--config`（Linux 默认 `/etc/vrndisd.conf`，Windows 默认 `%ProgramData%\vrndisd\vrndisd.conf`），
JSON 格式可手工编辑后 `service stop && service start` 生效。

### 在线管理功能

```bash
# 获取在线客户端列表
# 服务器运行时调用 GetClients() API

# 踢出指定客户端
# srv.KickClient("01:02:03:04:05:06")

# 发送管理命令
# srv.SendCommandToClient("01:02:03:04:05:06", cmd)
```

### STUN 服务器（P2P 连接支持）

```bash
# 构建并运行 STUN 服务器
./build/build.sh all

# 确保有 WebSocket 支持，然后运行 STUN 服务器
./build/bin/stun-server/stun-server --listen :5478
```

## 构建

### 交叉编译

```bash
./build/build.sh all           # 构建所有架构
./build/build.sh arm64         # 仅 ARM64
./build/build.sh armv6         # 仅 ARMv6
./build/build.sh armv7         # 仅 ARMv7
./build/build.sh amd64         # 仅 amd64 (默认)
./build/build.sh clean         # 清理构建产物
```

### 构建产物

- `build/bin/amd64/vrndisd` - amd64 版本
- `build/bin/arm64/vrndisd` - arm64 版本
- `build/bin/armv6/vrndisd` - armv6 版本
- `build/bin/armv7/vrndisd` - armv7 版本
- `build/bin/stun-server/stun-server` - STUN 服务器

### Docker 构建

```bash
docker-compose -f build/docker-compose.arm.yml build --pull
```

### 构建版本号说明

所有二进制文件都嵌入了 **Git 提交哈希** 版本号，例如：

```
vrndisd 1.0.0+214215c (linux/amd64)
```

其中 `214215c` 是 Git 提交哈希的简写，便于版本追踪和调试。

## 测试

```bash
go test ./... -count=1                  # 单元 + 端到端 + 健壮性（L1-L3）
go test ./internal/server/ -v           # 详细输出
go test ./internal/proto/ -fuzz=FuzzReadMessage -fuzztime=30s   # L4 fuzz
sudo bash tests/stress-real.sh ./vrndisd   # L5 真实网卡压测
sudo bash tests/reconnect-loop.sh ./vrndisd 50   # 崩溃恢复
```

## 服务工具参考

| 命令 | 作用 |
|---|---|
| `vrndisd service install` | 写配置 + 注册服务 + 启动（Linux: systemd 单元；Windows: sc.exe + SCM） |
| `vrndisd service uninstall` | 停止 + 注销 + 清理 |
| `vrndisd service start/stop` | 启停 |
| `vrndisd service status` | installed/running 查询 |
| `vrndisd service-run --config X` | 服务入口（systemd/SCM 调用；也可前台调试） |

Linux 单元特性：`Restart=always`（3s 后拉起）、`TimeoutStopSec=10`（优雅退出）、
`WantedBy=multi-user.target`（开机自启）。Windows：`sc failure` 5s/10s/30s 三次重启，
服务进程通过 `advapi32` SCM API 接受 STOP/SHUTDOWN 控制。

## 安全提示

- 数据面**明文 TCP，无认证**——仅适用于可信网络；跨公网请套 SSH/WireGuard/TLS 隧道
- 服务以 root/管理员运行（TAP 需要），请限制可连接来源 IP
- 健壮性测试规划与已知加固待办见 `docs/robustness-plan.md`（鉴权、MAC 表老化、限速等）

## 文件结构

```
vrndis-go/
├── cmd/vrndisd/            # CLI：server | client | service | service-run | version
├── internal/
│   ├── proto/              # 线路协议（分帧 + JSON 控制消息，含 fuzz 测试）
│   ├── ethernet/           # 以太网帧/ARP（含 fuzz 测试）
│   ├── addr/               # 地址池（分配/回收/复用）
│   ├── tap/                # 网卡抽象：Linux TAP / Windows wintun / Fake（测试）
│   ├── ethswitch/          # 学习型二层交换机
│   ├── server/             # 服务器（含端到端 + 健壮性测试）
│   ├── client/             # 客户端
│   ├── service/            # 配置文件 + systemd/SCM 服务管理
│   └── stun/               # STUN 服务器实现（P2P 连接支持）
├── build/
│   ├── build.sh            # 统一构建脚本 (含鸿蒙_PIGO 版本)
│   ├── Makefile            # Make 构建接口
│   ├── Dockerfile.arm      # ARM Docker 构建
│   └── docker-compose.arm.yml
├── tests/
│   ├── stress-real.sh      # 真实压测脚本
│   └── reconnect-loop.sh   # 崩溃恢复测试
├── docs/
│   ├── robustness-plan.md
│   └── windows.md
└── README.md
```

这是基于原始 vrndis-go 项目的增强版本，添加了对客户端管理和 STUN 的支持。
