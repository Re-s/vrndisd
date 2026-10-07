# vrndis 健壮性测试规划

> 目标：验证 vrndis（Python 版 / Go 版）在**异常输入、资源压力、故障恢复、长期运行**下的行为
> 可预期、可恢复、不崩溃、不泄漏。本文档既是测试设计，也是 Go 版自动化测试的索引。

## 0. 测试分层

| 层级 | 手段 | 入口 | 何时跑 |
|---|---|---|---|
| L1 单元测试 | Go `testing` + 表驱动 | `go test ./...` | 每次提交 |
| L2 集成/端到端 | 内存网卡 Fake + 真实 TCP | `go test ./internal/server/` | 每次提交 |
| L3 健壮性 | 本文档 §2-§6 自动化用例 | `go test -run Robust ./internal/server/` | 每次提交 |
| L4 模糊测试 | Go 原生 fuzz | `go test -fuzz=Fuzz ./internal/proto/ ./internal/ethernet/` | 每日 / 发版前 |
| L5 真实网卡压测 | Linux TAP + netns + iperf3/ping | `tests/stress-real.sh`（测试服务器） | 发版前 |
| L6 长稳 | 服务模式 24h+ 连接风暴 | systemd/SCM 常驻 + 定时重连脚本 | 发版前 |

## 1. 测试环境矩阵

| 维度 | 取值 |
|---|---|
| 平台 | Linux amd64（主）、Windows amd64（服务工具 + wintun 数据面） |
| Python 版 | 3.8 / 3.13 |
| Go 版 | 1.21+（实测 1.24） |
| 网卡后端 | Linux TAP、wintun（Windows）、Fake（测试） |
| 部署形态 | 前台进程、systemd 服务、Windows 服务 |

## 2. 协议健壮性（L3 已自动化 ✅）

| # | 用例 | 预期行为 | Go 测试 |
|---|---|---|---|
| 2.1 | 畸形以太网帧（空/过短/随机字节） | 静默丢弃该帧，会话不断、不影响后续合法帧 | `TestMalformedFramesDoNotKillSession` |
| 2.2 | 非 JSON 的 HELLO body | 容错忽略，数据面正常 | `TestGarbageHelloTolerated` |
| 2.3 | 未知消息类型 | 忽略（向前兼容），不断连 | `TestGarbageHelloTolerated` |
| 2.4 | 声明超长 body（>256KiB） | 关闭**该**连接，服务器与其它会话不受影响 | `TestOversizedMessageKillsOnlyThatSession` |
| 2.5 | 半条消息（截断 header/body） | 读阻塞或 EOF 处理，不 panic、不死等 | `TestEOFAndPartialData`（L1） |
| 2.6 | 模糊输入（随机字节流） | 永不 panic，只返回错误 | `FuzzReadMessage`、`FuzzParseFrame`（L4） |

## 3. 连接与会话健壮性（L3 已自动化 ✅）

| # | 用例 | 预期行为 | Go 测试 |
|---|---|---|---|
| 3.1 | 100 次快速连/断（连接风暴） | 无资源泄漏，地址池回到初始状态 | `TestConnectionChurnNoLeak` |
| 3.2 | 同一"槽位"30 次重连 | 每次复用同一地址，无累积 | `TestReconnectSameSlotStable` |
| 3.3 | 服务器停止 | 所有客户端在 2s 内感知断开 | `TestServerStopDisconnectsClients` |
| 3.4 | 客户端异常断电（TCP 中断） | 服务器回收租约、MAC 表项；重连即恢复 | `TestLeaseReleasedOnDisconnect`（L2）+ 真实环境 kill -9 |
| 3.5 | 协程/线程泄漏 | 30 轮上下线后协程数回到基线 | `TestGoroutinesReturnToBaseline` |

## 4. 资源与背压（L3 已自动化 ✅）

| # | 用例 | 预期行为 | Go 测试 |
|---|---|---|---|
| 4.1 | 地址池耗尽（/30 池） | 新客户端被拒绝且收到明确失败；释放后恢复 | `TestPoolExhaustionAndRecovery` |
| 4.2 | 慢消费者（客户端不读卡） | 只丢**该客户端**的帧（有界队列 512），其他客户端与新接入不受影响 | `TestSlowConsumerDoesNotBlockOthers` |
| 4.3 | 稳态节奏并发单播（9 发 1 收 ×50 帧，~500fps/发送者） | **零丢帧**，100% 送达 | `TestConcurrentUnicastLoadPaced` |
| 4.3b | 极端微爆发（8ms 内 900 帧） | 允许背压丢帧（交换机过载语义），但不得死锁/饿死/服务不可用 | `TestBurstNeverDeadlocks` |
| 4.4 | 8KiB 大帧透传 | 逐字节一致 | `TestLargeFramePassthrough` |
| 4.5 | 双重释放租约 | 池不膨胀、无 double-free | `TestDoubleReleaseSafe`（L1） |
| 4.6 | MAC 表膨胀（伪造源 MAC 洪泛） | 见 §7 待办：需加 MAC 表上限 + 老化 | 规划中 |

**背压策略（设计保证）**：每个客户端会话持有 512 帧有界发送队列；队列满时丢帧计数，
交换机永不因单个慢客户端阻塞。真实 TAP 写入由内核消费，不存在同等问题。
丢帧行为与真实交换机一致：过载端口丢帧而非阻塞全网（实测 8ms 微爆发下送达率 ~30%，
稳态节奏流量 100% 送达）。

## 5. 故障恢复（L3 部分自动化 ✅）

| # | 用例 | 预期行为 | 状态 |
|---|---|---|---|
| 5.1 | 服务进程崩溃 | systemd `Restart=always` / SCM `failure actions` 自动拉起 | 服务工具实测（Linux）|
| 5.2 | 服务器重启 | 客户端感知断开 → 重连 → 拿到地址 → 通信恢复 | 真实环境脚本 `tests/reconnect-loop.sh` |
| 5.3 | 服务器重启后地址池状态 | 全新分配，无陈旧租约 | 同上 |
| 5.4 | 网络闪断（隧道 TCP 断） | 客户端报错退出/重连；不 panic 不挂死 | `TestServerStopDisconnectsClients` |
| 5.5 | 机器重启 | systemd enable / SCM auto → 服务自动恢复 | 真实环境验证（见 §8 待办） |

## 6. 平台与部署（服务工具）

| # | 用例 | Linux | Windows |
|---|---|---|---|
| 6.1 | install → status=running | systemd 实测 ✅ | sc.exe 编译验证 + 待 Windows 主机实测 |
| 6.2 | stop / start 循环 ×10 | 实测 ✅ | 待测 |
| 6.3 | uninstall 后无残留（unit/注册表/进程） | 实测 ✅ | 待测 |
| 6.4 | 配置文件错误（非法 JSON / mode 错误） | 明确报错退出，服务 fail 状态可查 | 同 |
| 6.5 | 非 root / 非管理员运行 | 明确报错（TAP 权限） | 同 |

## 7. 已知待办（后续加固项）

1. **MAC 表上限与老化**：伪造源 MAC 洪泛可撑爆交换机表（内存耗尽缓慢）。
   计划：表上限（如 4096）+ 300s 老化 + LRU 淘汰。
2. **鉴权与加密**：当前明文 TCP 无认证（已在 README 标注）。计划：TLS + 预共享密钥握手。
3. **保活**：KEEPALIVE 消息已定义但未启用主动心跳；NAT 环境长连接需心跳保活。
4. **Windows 运行时验证**：wintun 数据面 + SCM 接入均已交叉编译通过，
   需要 Windows 主机跑 §6 矩阵（wintun.dll 随包分发）。
5. **限速**：单客户端发送速率无上限（内网场景可接受；公网需令牌桶）。

## 8. 执行手册

```bash
# L1-L3（每次提交）
go test ./... -count=1

# L3 带详细输出
go test ./internal/server/ -v -count=1

# L4 模糊测试（每个 30 秒）
go test ./internal/proto/ -fuzz=FuzzReadMessage -fuzztime=30s
go test ./internal/ethernet/ -fuzz=FuzzParseFrame -fuzztime=30s

# L5 真实网卡（root）
sudo bash tests/stress-real.sh          # netns 多客户端 + 吞吐 + 丢包
sudo bash tests/reconnect-loop.sh       # 100 次 kill -9 重连循环

# L6 长稳（服务模式）
sudo ./vrndisd service install --mode server --listen 0.0.0.0:7777
# 观察 24h：内存曲线（pidstat -r）、连接数、丢帧计数
```

**通过标准**：L1-L3 全绿；L4 fuzz 30min 无 crash；L5 丢包 0%、重连 100/100 成功；
L6 24h 内存增长 < 5% 且无 goroutine/线程泄漏。
