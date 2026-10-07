#!/bin/bash

echo "=== VRNDIS-Go 基本连接示例 ==="
echo "此脚本将启动服务器和客户端"

# 清理旧进程
pkill -f vrndisd 2>/dev/null
sleep 1

# 检查用户权限
if [ "$EUID" -ne 0 ]; then 
    echo "请使用 sudo 运行此脚本"
    exit 1
fi

# 启动服务器
echo "启动服务器..."
./build/bin/amd64/vrndisd server \
    --listen 0.0.0.0:7777 \
    --network 10.77.0.0/24 \
    --gateway 10.77.0.1 \
    --tap-name vrndis-srv \
    --name server &

SERVER_PID=$!

# 等待服务器就绪
sleep 2

echo "服务器已启动 (PID: $SERVER_PID)"

# 启动客户端
echo "启动客户端..."
./build/bin/amd64/vrndisd client \
    --server 127.0.0.1:7777 \
    --name my-client \
    --tap-name vrndis-c0 \
    --default-route &

CLIENT_PID=$!

echo "客户端已启动 (PID: $CLIENT_PID)"

# 显示状态
echo ""
echo "=== 系统状态 ==="
./build/bin/amd64/vrndisd status --server 127.0.0.1:7777 2>/dev/null || echo "状态查询失败"
echo ""
./build/bin/amd64/vrndisd clients --server 127.0.0.1:7777 2>/dev/null || echo "客户端列表查询失败"

# 显示 IP 地址
echo ""
echo "=== 网络接口 ==="
ip addr show | grep -E "vrndis|lo"

# 保存 PID
echo $SERVER_PID > /tmp/server.pid
echo $CLIENT_PID > /tmp/client.pid

echo ""
echo "示例启动完成！运行 cleanup.sh 清理。"

# 添加清理函数
cleanup() {
    echo "清理中..."
    pkill -f vrndisd 2>/dev/null
    rm -f /tmp/server.pid /tmp/client.pid
    iptables -F 2>/dev/null || true
    echo "清理完成"
}
trap cleanup EXIT

