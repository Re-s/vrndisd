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
  - MAC 表上限 4096 + 300s 老化 + LRU 淘汰（防源 MAC 洪泛撑爆表）
- **安全**：可选 PSK 鉴权加密（`--psk`，X25519 ECDH + AES-256-GCM，控制面消息整体加密）
- **保活心跳**：客户端周期 KEEPALIVE，NAT 环境长连接不断
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

# 常用选项（server / client 各自完整列表见 vrndisd server -h / vrndisd client -h）：
#   --psk yourpsk      可选，控制面加密鉴权（X25519 + AES-256-GCM，客户端须同 psk）
#   --stun             仅 server：同时启动内置 STUN（P2P 打洞/netcheck 依赖，默认 :5478/:5479）
#   --p2p              仅 client：P2P 直连（NAT 穿透走 UDP，失败自动回退服务器中继）
#   --default-route    仅 client：默认路由指向隧道（VPN 模式，全部流量走隧道）
```

### 服务器侧：转发与 NAT（--default-route 走外网的前提）

若希望客户端经隧道访问外网，服务器除 `--listen` 外还需要内核转发 +
防火墙规则（vrndisd 不自动操作 iptables，交由运维用熟悉的方式管理）：

```bash
sysctl -w net.ipv4.ip_forward=1
iptables -I FORWARD 1 -i vrndis0 -j ACCEPT     # 放行隧道入站转发
iptables -I FORWARD 1 -o vrndis0 -j ACCEPT     # 放行返程
iptables -t nat -A POSTROUTING -s 10.77.0.0/24 -j MASQUERADE   # SNAT 回包
```

规则务必持久化（否则重启丢失）。生产机可用幂等脚本 + oneshot 服务，
脚本示例见仓库 `deploy/vrndis-firewall.sh`。

## Android（Termux + root）

在 Termux 中以 root 运行 vrndisd（需自行交叉编译 `build/build.sh android`，
或取发行二进制），即可获得与 Linux 相同的二层 TAP 隧道：

```bash
# 手机（root shell）
./vrndisd client --server 服务器IP:7777
```

说明：
- `Up()` 会自动插入 `ip rule add to <隧道网段> lookup main prio 6091`，
  绕过 Android netd 的 fwmark 路由锁定（否则本地流量被锁进物理网卡专用
  路由表，ping 网关会被劫持去 wlan0，即使 main 表路由正确也不生效）。
  规则命中但 main 表无对应路由时会 fall through，故网卡关闭后残留安全。
- 32 位 Android(arm) 需 NDK cgo，仅提供 arm64 构建。
- 若需要全局走隧道（VPN 模式），应使用 VpnService 形式的客户端，
  Termux 裸 TAP 只能保证"隧道子网可达"。

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

# Windows（管理员 CMD/PowerShell；wintun.dll 已内嵌进 exe，
# 首次运行自动释放到 exe 同目录，单文件部署）
vrndisd.exe service install --mode server --listen 0.0.0.0:7777
vrndisd.exe service status
vrndisd.exe service uninstall
```

配置写入 `--config`（Linux 默认 `/etc/vrndisd.conf`，Windows 默认 `%ProgramData%\vrndisd\vrndisd.conf`），
JSON 格式可手工编辑后 `service stop && service start` 生效。

### 在线管理功能（status / clients）

服务器运行时，在本机即可查询（经管理套接字，免输地址；远程主机需把
`--socket` 指向服务器平台默认路径）：

```bash
# 服务器运行状态
vrndisd status
# 状态: 版本=1.1.0+19405ee 在线客户端=4

# 在线客户端列表（地址 / 名称 / MAC / 对端 / P2P 路径）
vrndisd clients
# 在线客户端列表 (4)：
#   1. 10.77.0.2 (win-test/02:00:00:00:00:01) -> 198.51.100.10:54404 [p2p: relay]
#   2. 10.77.0.3 (nuc/02:00:00:00:00:02)     -> 198.51.100.20:40609 [p2p: relay]
#   3. 10.77.0.4 (x16d-dbg/02:00:00:00:03)   -> 198.51.100.20:23798 [p2p: stun-ok 198.51.100.20:33129（打洞中）]
#
# p2p 字段含义：relay=走服务器中继；stun-ok <addr>=已拿到 STUN 映射候选并尝试直连

### P2P 诊断（netcheck / ping / peers）

仿 Tailscale 的三件套，在任何客户端机器上即可自测 P2P 能力：

```bash
# 1) netcheck：本机 NAT 环境与 UDP 可达性探测
vrndisd netcheck --server 203.0.113.1 --extra-stun stun.l.google.com:19302
# 输出各 STUN 返回的映射地址、MappingVariesByDest（是否 symmetric NAT）、
# NAT 类型与打洞预期，并附"结论"段直接给出预期路径（P2P 优先 / 服务器中继）
# 与可执行建议（如建议加 --extra-stun、提示 --server 端口误填）。
# 主/第二 STUN 指向服务器自建 :5478 / :5479，--extra-stun 可选第三方 STUN
# 作交叉比对。

# 2) ping：到指定 peer 的路径探测（relay vs p2p + RTT）
vrndisd ping 10.77.0.4 --count 5
# --server/--psk 可省略：回退读已安装服务配置（vrndisd service install 写入的
# /etc/vrndisd.conf 或 %ProgramData%\vrndisd\vrndisd.conf）；
# 本机即服务器时自动用 listen 地址回环直连。显式指定仍然优先：
#   vrndisd ping 10.77.0.4 --server 203.0.113.1:7777 --psk yourpsk
# 若本地到该 peer 的直连路径可通，输出 pong from 10.77.0.4 via p2p(1.2.3.4:5678) in 8ms；
# 否则输出 via relay 的中继基准 RTT。

# 3) peers：全部在线 peer 的路径一览
vrndisd peers
# 在线 peer (3)：
#   10.77.0.5              via relay  250µs
#   10.77.0.2 (win-test)   via relay  250µs
#   10.77.0.4 (x16d-dbg)   via relay  250µs
# 提示: 本轮无任何直连回包，全部经服务器中继；netcheck 命令可评估本机打洞能力
# （ping 同理：全程无直连回包时追加一行相同提示）
```

实现要点：`internal/netcheck` 用同一 UDP socket 串行向多个 STUN 发 Binding
Request 并比对映射端口/IP，判定 cone / symmetric / blocked；`internal/ctrl`
是到服务器的轻量控制信道（复用 Hello/Assign 握手，不建网卡不进数据面），
ping/peers 经 PeerQuery 拿对端候选后，用 p2p 的 disco 探测帧（probe magic）
直接向候选地址测直连 RTT——对端 `Tunnel.Read` 收到任意来源的 probe 会原路回包。

### STUN 服务器（P2P 连接支持）

内置 STUN（RFC 3489/5389 Binding），两种用法：

```bash
# 1) 随服务器一并启动（推荐，一条命令搞定 netcheck + 打洞）
sudo ./vrndisd server --listen 0.0.0.0:7777 --stun
# --stun-listen / --stun-listen-extra 改监听端口（默认 :5478 / :5479；
# netcheck 依赖同服务器双端口比对映射，两个端口都要对客户端放行 UDP）

# 2) 独立 STUN（不跑隧道服务器）
sudo ./vrndisd stun-server --listen 0.0.0.0:5478
# systemd 单元示例见 deploy/stun-server.service
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

- `build/bin/amd64/vrndisd`      - linux/amd64
- `build/bin/arm64/vrndisd`      - linux/arm64（含树莓派 3/4 64 位）
- `build/bin/armv6/vrndisd`      - linux/armv6
- `build/bin/armv7/vrndisd`      - linux/armv7
- `build/bin/windows/vrndisd.exe` - windows/amd64（wintun.dll 已内嵌）
- `build/bin/android-arm64/vrndisd` - android/arm64（Termux + root）

### Docker 构建

```bash
docker-compose -f build/docker-compose.arm.yml build --pull
```

### 构建版本号说明

所有二进制文件都嵌入了 **Git 提交哈希** 版本号，例如：

```
vrndisd 1.1.0+19405ee (linux/amd64)
```

其中 `214215c` 是 Git 提交哈希的简写，便于版本追踪和调试。

## 测试

```bash
go test ./... -count=1                  # 单元 + 端到端 + 健壮性（L1-L3）
go test ./internal/server/ -v           # 详细输出
go test ./internal/proto/ -fuzz=FuzzReadMessage -fuzztime=30s   # L4 fuzz
sudo bash tests/stress-real.sh ./vrndisd   # L5 真实网卡压测
sudo bash tests/reconnect-loop.sh ./vrndisd 50   # 崩溃恢复
sudo bash tests/soak.sh ./vrndisd 24h           # 长稳（RSS/连接漂移观测）
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
- 健壮性测试规划见 `docs/robustness-plan.md`（鉴权加密、MAC 表老化等加固项已全部实现并验证）

## 文件结构

```
vrndis-go/
├── cmd/vrndisd/            # CLI：server | client | service | service-run | stun-server
│                           #       netcheck | ping | peers | status | clients | version
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
│   ├── build.sh            # 统一交叉构建脚本
│   ├── Makefile            # Make 构建接口
│   ├── Dockerfile.arm      # ARM Docker 构建
│   └── docker-compose.arm.yml
├── tests/
│   ├── stress-real.sh      # 真实压测脚本
│   ├── reconnect-loop.sh   # 崩溃恢复测试
│   └── soak.sh             # 24h 长稳测试
├── docs/
│   ├── robustness-plan.md
│   └── windows.md
└── README.md
```

这是基于原始 vrndis-go 项目的增强版本，添加了对客户端管理和 STUN 的支持。
