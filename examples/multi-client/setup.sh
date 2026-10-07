#!/bin/bash

echo "=== VRNDIS-Go 多客户端网络示例 ==="
echo "此脚本将启动服务器和多个客户端"

# 清理旧进程
pkill -f vrndisd 2>/dev/null
sleep 1

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
    --name vrndis-server &

SERVER_PID=$!
echo "服务器已启动 (PID: $SERVER_PID)"

# 等待服务器就绪
sleep 2

# 启动客户端队列 (根据配置数量启动)
echo "启动客户端队列..."
CLIENT_NAMES=("client-a" "client-b" "client-c")
TAP_NAMES=("vrndis-c0" "vrndis-c1" "vrndis-c2")
IPS=("10.77.0.2" "10.77.0.3" "10.77.0.4")

for i in "${!CLIENT_NAMES[@]}"; do
    echo "启动 ${CLIENT_NAMES[$i]} (${IPS[$i]} / $i)..."
    ./build/bin/amd64/vrndisd client \
        --server 127.0.0.1:7777 \
        --name "${CLIENT_NAMES[$i]}" \
        --tap-name "${TAP_NAMES[$i]}" &
done

# 显示状态
echo ""
echo "=== 系统状态 ==="
./build/bin/amd64/vrndisd status --server 127.0.0.1:7777 2>/dev/null || echo "状态查询失败"
echo ""
./build/bin/amd64/vrndisd clients --server 127.0.0.1:7777 2>/dev/null || echo "客户端列表查询失败"

echo ""
echo "=== 网络接口 ==="
ip addr show | grep -E "vrndis"

echo ""
echo "多客户端网络示例启动完成！"
echo "运行 cleanup.sh 清理环境。"

# 记录所有 PID
echo $$SERVER_PID > /tmp/vrndis_multi_server.pid
ps aux | grep -f "/tmp/vrndis_multi_server.pid"

