# 管理命令使用示例

本文档介绍 VRNDIS-Go 的服务器管理命令使用方法和最佳实践。

## 可用命令

1. **status** - 查询服务器状态和版本信息
2. **clients** - 列出所有在线客户端
3. **kick** - 踢出指定客户端
4. **ping** - 测试客户端连接

## 命令使用

### 1. 状态查询

```bash
# 基础状态查询
./build/bin/amd64/vrndisd status --server 127.0.0.1:7777

# 详细输出示例：
# 状态: 版本=1.0.0+716a8f2 在线客户端=3
```

### 2. 客户端列表

```bash
# 获取在线客户端列表
./build/bin/amd64/vrndisd clients --server 127.0.0.1:7777

# 输出示例：
# 在线客户端列表 (3):
#   1. client-a (02:00:00:00:00:01) - 127.0.0.1:36506
#   2. client-b (02:00:00:00:00:02) - 127.0.0.1:36508
#   3. client-c (02:00:00:00:00:03) - 127.0.0.1:36510
```

### 3. 踢出客户端

```bash
# 移除指定的客户端
./build/bin/amd64/vrndisd client \
    --server 127.0.0.1:7777 \
    --client client-b \
    --action kick

# 输出：
# 操作结果：客户端 client-b 已成功移除
# 当前在线客户端: 2
```

### 4. 测试客户端连接

```bash
# 测试客户端响应
./build/bin/amd64/vrndisd client \
    --server 127.0.0.1:7777 \
    --client client-a \
    --action ping

# 输出：
# 客户端 client-a 响应时间: 15ms
# 状态: 在线
```

## 使用场景

### 场景 1: 日常监控
```bash
# 每分钟查询状态
watch -n 60 'vrndisd status --server 127.0.0.1:7777'

# 每小时生成报告
for hour in {0..23}; do
    echo "时间: $hour:00" > report.log
    vrndisd status --server 127.0.0.1:7777 >> report.log
    vrndisd clients --server 127.0.0.1:7777 >> report.log
    sleep 3600
done
```

### 场景 2: 客户端管理
```bash
# 批量操作脚本
#!/bin/bash

# 启动服务器
./build/bin/amd64/vrndisd server \
    --listen 0.0.0.0:7777 \
    --network 10.77.0.0/24 \
    --gateway 10.77.0.1 &

sleep 2

# 添加客户端
./build/bin/amd64/vrndisd client --server 127.0.0.1:7777 --name client-a &
./build/bin/amd64/vrndisd client --server 127.0.0.1:7777 --name client-b &

# 查询在线客户端
clients=$(vrndisd clients --server 127.0.0.1:7777 | grep -c "client-")
echo "当前在线客户端数: $clients"

# 如果有人连接超过 5 分钟后，移除不活跃客户端
sleep 300
for client in $(vrndisd clients --server 127.0.0.1:7777 | grep -oE "client-[0-9]+"); do
    # 检查连接是否活跃
    last_active=$(...)  # 实现检查逻辑
    if [ $last_active -gt 300 ]; then
        echo "移除不活跃客户端: $client"
        vrndisd client --server 127.0.0.1:7777 --client $client --action kick
    fi
done
```

### 场景 3: 集成到其他系统

```bash
# 在 Ansible 中使用
- name: 检查 VRNDIS 状态
  uri:
    url: http://127.0.0.1:7777/status
    method: GET
  register: status

- name: 记录状态
  debug:
    msg: "VRNDIS 服务状态={{ status.json.status }}"

# 在 Prometheus 监控中使用
#!/bin/bash
# /usr/local/bin/vrndis_metrics.sh

STATUS=$(./build/bin/amd64/vrndisd status --server 127.0.0.1:7777)
CLIENTS=$(./build/bin/amd64/vrndisd clients --server 127.0.0.1:7777)

echo "vrndis_online_clients $(echo $CLIENTS | grep -oE '[0-9]+')"
echo "vrndis_version $(echo $STATUS | grep -oE '1\.[0-9]+\.[0-9]+')"
```

## 返回格式

### status 命令输出
```json
{
  "version": "1.0.0+716a8f2",
  "clients": 3,
  "uptime": "1h23m45s",
  "errors": []
}
```

### clients 命令输出
```json
{
  "version": "1.0.0+716a8f2",
  "clients": 3,
  "list": [
    {
      "ip": "10.77.0.2",
      "mac": "02:00:00:00:00:01",
      "peer": "127.0.0.1:36506",
      "name": "client-a"
    },
    {
      "ip": "10.77.0.3",
      "mac": "02:00:00:00:00:02",
      "peer": "127.0.0.1:36508",
      "name": "client-b"
    }
  ]
}
```

## 最佳实践

1. **定期查询**: 每 60 秒查询一次状态监控
2. **参数验证**: 确保客户端名称正确
3. **错误处理**: 捕获命令执行失败的异常情况
4. **资源限制**: 设置合理的超时时间
5. **权限控制**: 限制管理命令的访问权限

## 安全性

- 管理命令通过 TCP 连接
- 建议绑定到本地地址或添加身份验证
- 生产环境建议启用 TLS 加密
- 定期更新服务以修复安全漏洞

## 故障排查

```bash
# 如果命令失败，检查：
./build/bin/amd64/vrndisd status --server 127.0.0.1:7777 --verbose

# 查看服务器日志
journalctl -u vrndis-server -f

# 检查网络连接
nc -vz 127.0.0.1 7777

# 测试端口连通性
telnet 127.0.0.1 7777
```

## 性能优化

```bash
# 使用缓存减少查询频率
CACHE_TTL=60
if [ $(date +%s) -lt $(($CACHE_FILE + $CACHE_TTL)) ]; then
    cat $CACHE_FILE
else
    ./build/bin/amd64/vrndisd status --server 127.0.0.1:7777 > $CACHE_FILE
    $(date +%s) > $CACHE_TIME
fi
```

更多详细信息请查看源代码和文档。
