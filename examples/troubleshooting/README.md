# 故障排查示例

本文档提供常见的 VRNDIS-Go 问题及其解决方案，帮助快速定位和修复故障。

## 快速诊断脚本

```bash
#!/bin/bash
# vrndis_diagnostic.sh

echo "=== VRNDIS-Go 诊断脚本 ==="

# 1. 服务状态检查
echo "1. 服务状态检查..."
if command -v systemctl >/dev/null 2>&1; then
    systemctl status vrndis-server --no-pager 2>/dev/null | head -20
else
    service vrndis-server status 2>/dev/null || echo "服务未运行"
fi

# 2. 端口检查
echo -e "\n2. 端口检查..."
ss -tlnp | grep 7777 || echo "端口 7777 未被占用"

# 3. 日志检查
echo -e "\n3. 日志检查..."
if command -v journalctl >/dev/null 2>&1; then
    journalctl -u vrndis-server -n 50 --no-pager
else
    echo "未找到 journalctl"
fi

# 4. 网络连通性
echo -e "\n4. 网络连通性测试..."
nc -vz 127.0.0.1 7777 && echo "TCP 7777 端口可连接" || echo "端口连接失败"

# 5. 内存和 CPU 使用
echo -e "\n5. 资源使用..."
ps aux | grep vrndisd || echo "无 vrndisd 进程"

# 6. TAP 设备
echo -e "\n6. TAP 设备检查..."
ip link show | grep -E "vrndis|tap" || echo "VRNDIS TAP 设备未找到"

# 7. 版本信息
echo -e "\n7. 版本信息..."
./build/bin/vrndisd version 2>/dev/null || echo "无法获取版本"

# 8. 客户端连接测试
echo -e "\n8. 客户端连接测试..."
./build/bin/vrndisd status --server 127.0.0.1:7777 2>/dev/null || echo "状态查询失败"

echo -e "\n诊断完成！"
```

## 常见问题

### 1. 服务器无法启动

**症状**: `listen tcp 0.0.0.0:7777: bind: address already in use`

**原因**: 端口 7777 被其他程序占用。

**解决方案**:
```bash
# 查找占用进程
lsof -i :7777
# 或
ss -tlnp | grep 7777

# 终止占用进程
kill -9 $(lsof -t -i :7777)

# 或修改启动端口
./build/bin/amd64/vrndisd server --listen 0.0.0.0:7778
```

**预防措施**: 修改 `.gitignore` 或配置中设置非标准端口。

### 2. 客户端连接超时

**症状**: 客户端连接失败，超时。

**原因**: 防火墙拦截、网络路径不可达、服务器未启动。

**排查步骤**:
```bash
# 1. 检查服务器是否运行
ps aux | grep vrndisd

# 2. 测试网络连通性
ping 服务器IP
nc -zv 服务器IP 7777

# 3. 检查防火墙
iptables -L -n | grep 7777
# 或
ufw status | grep 7777

# 4. 检查路由
traceroute 服务器IP
```

**解决方案**:
- 开启防火墙规则
- 确保网络路径可达
- 重启服务器

### 3. TAP 设备权限问题

**症状**: `open /dev/net/tun: permission denied`

**原因**: 当前用户没有访问 TAP 设备的权限。

**解决方案**:
```bash
# 临时解决 (当前 session)
sudo setcap cap_net_admin=cip ./build/bin/vrndisd

# 永久解决 (添加用户组)
sudo usermod -aG netdev $USER
# 或
sudo adduser $USER netdev

# 重启应用程序
```

**验证**:
```bash
ls -l /dev/net/tun
```

### 4. NAT 功能异常

**症状**: 客户端之间无法互通，TAP 设备配置失败。

**排查**:
```bash
# 1. 检查 iptables 规则
iptables -t nat -L POSTROUTING -n
iptables -L FORWARD -n

# 2. 检查 IP 转发
sysctl net.ipv4.ip_forward
echo 1 | sudo tee /proc/sys/net/ipv4/ip_forward

# 3. 启用 NAT (如果未启用)
iptables -t nat -A POSTROUTING -o eth0 -j MASQUERADE
iptables -A FORWARD -m state --state RELATED,ESTABLISHED -j ACCEPT
iptables -A FORWARD -i vrndis-srv -o eth0 -j ACCEPT
```

### 5. 性能问题

**症状**: 高延迟、低吞吐量。

**排查**:
```bash
# 1. 检查系统负载
top -bn1 | head -20

# 2. 检查网络接口流量
iftop -i vrndis-srv
# 或
nethogs

# 3. 测试网络性能
iperf3 -c 10.77.0.1 -p 5001

# 4. 检查内存使用
free -h

# 5. 检查磁盘 I/O
iostat -xz 1 3
```

**优化建议**:
- 增加系统内存
- 使用 SSD
- 优化系统网络参数
- 减少其他网络应用

### 6. STUN 服务器问题

**症状**: NAT 打洞失败，P2P 连接不上。

**排查**:
```bash
# 1. 检查 STUN 服务器端口
ss -ulnp | grep 5478

# 2. 测试 STUN 响应
nmap -sU -p 5478 服务器IP
# 或
stund -p 5478 -c 服务器IP

# 3. 检查 UDP 流量
tcpdump -i any port 5478 -n
```

**常见问题**:
- UDP 端口 5478 被防火墙拦截
- NAT 类型限制
- 网络busy导致丢包

### 7. 客户端连接不稳

**症状**: 客户端频繁断开重连。

**排查**:
```bash
# 1. 查看连接日志
journalctl -u vrndis-server -f

# 2. 分析网络稳定性
ping -i 0.2 网关IP
# 或
mtr 网关IP
```

**解决方案**:
- 增加心跳检测频率
- 优化连接超时参数
- 提高系统资源

## 调试模式

启用详细日志进行深度调试：

```bash
# 设置详细日志
export VRNDIS_LOG_LEVEL=debug

# 或启动时指定
./build/bin/amd64/vrndisd server --log-level debug

# 在线查看日志
tail -f /var/log/vrndis/vrndis.log
```

## 自动收集诊断信息

```bash
#!/bin/bash
# collect_diagnostic_info.sh

echo "=== VRNDIS-Go 诊断信息收集 ==="
echo "时间: $(date)"
echo "版本: $(./build/bin/vrndisd version)"

{
    echo "=== 服务状态 ==="
    systemctl status vrndis-server --no-pager 2>/dev/null
    
    echo "=== 端口状态 ==="
    ss -tlnp | grep 7777
    
    echo "=== 客户端连接 ==="
    ./build/bin/vrndisd clients --server 127.0.0.1:7777 2>/dev/null
    
    echo "=== 资源使用 ==="
    ps aux | grep vrndisd
    
    echo "=== TAP 设备 ==="
    ip link show | grep vrndis
    
    echo "=== 最近日志 ==="
    journalctl -u vrndis-server -n 100 --no-pager
    
    echo "=== 网络配置 ==="
    ip addr show
    ip route show
    iptables -t nat -L -n
    
    echo "=== 软件测试结果 ==="
    go test ./... 2>&1 | tail -20
    
} > diagnostic_report_$(date +%s).txt

echo "诊断报告已保存到: diagnostic_report_$(date +%s).txt"
```

## 技术支持

如果以上方法无法解决问题，请提供以下信息：

1. 诊断报告 (`diagnostic_report_*.txt`)
2. 系统信息 (`uname -a`)
3. 错误日志
4. 复现步骤

GitHub Issues: https://github.com/yourname/vrndis-go/issues
支持邮箱: support@vrndis.example.com

## 资源链接

- [官方文档](https://docs.vrndis.example.com)
- [社区论坛](https://community.vrndis.example.com)
- [视频教程](https://youtube.com/@vrndis)

更多帮助请查看项目 README.md 文件。
