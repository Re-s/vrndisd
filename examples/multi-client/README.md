# 多客户端网络示例

演示如何配置和管理包含多个客户端的虚拟网络。

## 场景概述

- **服务器**: 10.77.0.1 (网关)
- **客户端 A**: 10.77.0.2 (name: client-a)
- **客户端 B**: 10.77.0.3 (name: client-b)
- **客户端 C**: 10.77.0.4 (name: client-c)

## 网络配置

```
[客户端A:10.77.0.2] <--- [vrndis-go 服务器:10.77.0.1] ---> [客户端B:10.77.0.3]
                                  |
                          (10.77.0.0/24)
                                  |
                          [客户端C:10.77.0.4]
```

## 启动步骤

### 1. 启动服务器

```bash
./build/bin/amd64/vrndisd server \
    --listen 0.0.0.0:7777 \
    --network 10.77.0.0/24 \
    --gateway 10.77.0.1 \
    --name vrndis-server
```

### 2. 启动客户端 (每个客户端不同轮次)

```bash
# 客户端 A
./build/bin/amd64/vrndisd client \
    --server 127.0.0.1:7777 \
    --name client-a \
    --tap-name vrndis-c0

# 客户端 B
./build/bin/amd64/vrndisd client \
    --server 127.0.0.1:7777 \
    --name client-b \
    --tap-name vrndis-c1

# 客户端 C
./build/bin/amd64/vrndisd client \
    --server 127.0.0.1:7777 \
    --name client-c \
    --tap-name vrndis-c2
```

### 3. 验证客户端连接

```bash
# 查看在线客户端
./build/bin/amd64/vrndisd clients --server 127.0.0.1:7777

# 输出示例：
# 在线客户端列表 (3):
#   1. client-a (02:00:00:00:00:01) - 10.77.0.2
#   2. client-b (02:00:00:00:00:02) - 10.77.0.3
#   3. client-c (02:00:00:00:00:03) - 10.77.0.4
```

### 4. 测试通信

```bash
# 在客户端 A 测试连接所有客户端
ping -c 4 10.77.0.2  # client-b
ping -c 4 10.77.0.3  # client-c

# 在客户端 B 测试
ping -c 4 10.77.0.2  # client-a
ping -c 4 10.77.0.4  # client-c

# 在客户端 C 测试
ping -c 4 10.77.0.2  # client-a
ping -c 4 10.77.0.3  # client-b
```

## 管理命令

### 踢出客户端

```bash
# 移除指定的客户端
./build/bin/amd64/vrndisd client \
    --server 127.0.0.1:7777 \
    --client client-b \
    --action kick
```

### 保存服务器状态

```bash
# 查看服务器状态
./build/bin/amd64/vrndisd status --server 127.0.0.1:7777

# 输出：
# 状态: 版本=1.0.0+716a8f2 在线客户端=3
```

## 性能指标

- **同时连接数**: 无限制 (取决于系统资源)
- **网络吞吐量**: 每个客户端约 100-1000Mbps
- **延迟**: 典型值 < 10ms (局域网内)

## 应用场景

- **游戏服务器**: 多玩家虚拟网络
- **物联网模拟**: 多个设备互联互通
- **分布式系统测试**: 集群环境模拟
- **教学演示**: 网络拓扑展示
