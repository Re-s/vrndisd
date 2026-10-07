# Windows 部署说明

## 数据面：wintun

Windows 上网卡后端使用 [wintun](https://www.wintun.net/)（WireGuard 官方驱动，轻量 NDIS 适配器）：

1. 从官方渠道获取 `wintun.dll`（amd64），放到 `vrndisd.exe` **同目录**；
2. 以**管理员**权限运行（创建网卡需要）；
3. 首次运行会自动创建 wintun 适配器（名称即 `--tap` 参数，默认 `vrndis-c0`）。

差异说明：
- wintun 适配器的 **MAC 由驱动随机生成**，`--tap-mac` / ASSIGN 中的 MAC 仅作隧道侧标识
  （L2 转发仍以隧道内帧的源 MAC 为准，功能不受影响）；
- IP/路由配置通过 `netsh` 完成（已内置，无需手工操作）；
- MTU 默认 1500。

## 服务：SCM

`vrndisd.exe service install` 做三件事：
1. 写配置到 `%ProgramData%\vrndisd\vrndisd.conf`（可用 `--config` 改）；
2. `sc.exe create vrndisd binPath= "vrndisd.exe --config ... service-run" start= auto`，
   并设置 `description` 与 `failure`（崩溃后 5s/10s/30s 自动重启）；
3. `sc.exe start vrndisd`。

`vrndisd.exe service-run` 内置最小 SCM 接入（`StartServiceCtrlDispatcherW` /
`RegisterServiceCtrlHandlerExW` / `SetServiceStatus`，纯 syscall 无第三方依赖），
支持服务控制面板的停止/启动；若被 SCM 拉起失败（错误 1063）会自动退化为前台运行，便于调试。

## 验证状态

| 项 | 状态 |
|---|---|
| `GOOS=windows go build` 交叉编译 | ✅ 通过 |
| wintun 数据面收发 | ⏳ 需 Windows 主机运行时验证（wintun.dll 就位后跑 `tests/stress-real.sh` 等价手工步骤） |
| SCM 服务安装/停止/卸载 | ⏳ 同上（测试矩阵见 docs/robustness-plan.md §6） |

运行时验证清单（Windows 主机，管理员 PowerShell）：
```powershell
.\vrndisd.exe service install --mode server --listen 0.0.0.0:7777
.\vrndisd.exe service status        # 期望 installed=true running=true
sc.exe query vrndisd                # 期望 STATE: RUNNING
# 另一台/另一进程跑 client 验证拿地址与 ping
.\vrndisd.exe service stop
.\vrndisd.exe service uninstall
sc.exe query vrndisd                # 期望失败（已删除）
```
