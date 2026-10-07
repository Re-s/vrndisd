# STUN P2P 连接示例

此示例展示如何使用 STUN 服务器实现 NAT 打洞和 P2P 连接。

## 架构

```
[客户端A] <---STUN---> [STUN服务器] <---P2P---> [客户端B]
[位于 NAT 后]             [3478端口]             [位于 NAT 后]
```

## 组件

1. **STUN 服务器** - 处理 USend/响应，用于 NAT 探测
2. **VRNDIS 客户端 A** - 位于 NAT 后
3. **VRNDIS 客户端 B** - 位于 NAT 后

## 操作步骤

1. 启动 STUN 服务器：
   ```bash
   ./build/bin/stun-server/stun-server --addr :3478
   ```

2. 启动客户端 A (无公网 IP)：
   ```bash
   ./build/bin/amd64/vrndisd client \
     --server 192.168.1.100:7777 \
     --stun-server 192.168.1.100:3478 \
     --name client-a
   ```

3. 启动客户端 B (另一个 NAT 后)：
   ```bash
   ./build/bin/amd64/vrndisd client \
     --server 192.168.1.200:7777 \
     --stun-server 192.168.1.200:3478 \
     --name client-b
   ```

4. 验证 P2P 连接：
   ```bash
   # 在客户端 A 测试
   ping -c 4 10.77.0.2  # 客户端 B 的 IP
   ```

## STUN 服务器配置

### 基本配置
```
端口: 3478
协议: UDP
 Max 请求: 无限制
 区域名: vrndis
```

### 高级配置
```
编译参数:
  go build -ldflags "-X main.Version=1.0.0+716a8f2" -o stun-server ./cmd/stun-server
```

## 故障排查

### 1. STUN 连接失败
```bash
# 检查防火墙
iptables -L -n | grep 3478

# 检查 STUN 服务器状态
./build/bin/stun-server/stun-server --addr :3478 --verbose
```

### 2. NAT 打洞失败
- 确认 NAT 类型（全锥/对称/端口限制）
- 检查 UDP 流量是否阻塞
- 测试 STUN 反射器解析

### 3. P2P 连接不上
- 检查客户端日志中的 P2P 信息
- 确认服务器地址正确
- 验证 STUN 响应时间

## 网络拓扑

```
公网 IP: 203.0.113.1 STUN 服务器
      |
   客户端 A (10.0.1.100) <-- NAT --> 公网 Internet <-- NAT --> 客户端 B (10.0.2.100)
```

## 性能指标

- STUN 响应时间: < 100ms
- NAT 打洞成功率: > 95%
- P2P 建立时间: 2-5 秒
- 数据传输延迟: 取决于网络质量
