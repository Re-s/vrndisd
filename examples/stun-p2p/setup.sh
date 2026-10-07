#!/bin/bash

echo "=== VRNDIS-Go STUN P2P 示例 ==="
echo "此脚本将启动 STUN 服务器和 P2P 客户端队列"

# 清理旧进程
pkill -f stun-server 2>/dev/null
pkill -f vrndisd 2>/dev/null
sleep 1

if [ "$EUID" -ne 0 ]; then 
    echo "请使用 sudo 运行此脚本"
    exit 1
fi

# 启动 STUN 服务器
echo "启动 STUN 服务器..."
./build/bin/stun-server/stun-server --addr 0.0.0.0:3478 &
STUN_PID=$!
echo "STUN 服务器已启动 (PID: $STUN_PID)"

# 等待启动
sleep 1

# 添加 NAT 规则 (模拟)
iptables -t nat -A PREROUTING -p udp --dport 3478 -j ACCEPT 2>/dev/null || true
iptables -A INPUT -p udp --dport 3478 -j ACCEPT 2>/dev/null || true

# 启动客户端 (根据 IP 配置不同场景)
echo "客户端网络环境:"
echo "1. 客户端 A (10.0.1.0/24 - 内网)"
echo "2. 客户端 B (10.0.2.0/24 - 内网)"
echo "3. 启动全部"
read -p "请选择场景 [1-3]: " SCENARIO

case $SCENARIO in
    1)
        # 模拟客户端 A
        echo "启动客户端 A 模拟..."
        ./build/bin/amd64/vrndisd client \
            --server 192.168.1.100:7777 \
            --stun-server 192.168.1.100:3478 \
            --name client-a &
        ;;
    2)
        # 模拟客户端 B
        echo "启动客户端 B 模拟..."
        ./build/bin/amd64/vrndisd client \
            --server 192.168.1.200:7777 \
            --stun-server 192.168.1.200:3478 \
            --name client-b &
        ;;
    3)
        # 启动全部客户端
        echo "启动客户端 A..."
        ./build/bin/amd64/vrndisd client \
            --server 192.168.1.100:7777 \
            --stun-server 192.168.1.100:3478 \
            --name client-a &
        sleep 1
        echo "启动客户端 B..."
        ./build/bin/amd64/vrndisd client \
            --server 192.168.1.200:7777 \
            --stun-server 192.168.1.200:3478 \
            --name client-b &
        ;;
    *)
        echo "无效选项"
        exit 1
        ;;
esac

echo ""
echo "=== 当前状态 ==="
ps aux | grep -E "stun-server|vrndisd" | grep -v grep

echo ""
echo "STUN P2P 示例启动完成！"
echo "运行 cleanup.sh 清理环境。"
