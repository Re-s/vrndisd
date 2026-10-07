#!/bin/bash

echo "=== VRNDIS-Go 管理命令使用示例 ==="
echo "此脚本演示各种管理命令的组合使用"

if [ "$EUID" -ne 0 ]; then
    echo "请使用 sudo 运行此脚本"
    exit 1
fi

# 启动服务器
echo "启动管理测试服务器..."
./build/bin/amd64/vrndisd server \
    --listen 0.0.0.0:7777 \
    --network 10.77.0.0/24 \
    --gateway 10.77.0.1 \
    --name management-test &

SERVER_PID=$!
echo "服务器已启动 (PID: $SERVER_PID)"
sleep 2

# 初始化客户端队列
echo "初始化测试客户端..."
./build/bin/amd64/vrndisd client \
    --server 127.0.0.1:7777 \
    --name test-client-a \
    --tap-name vrndis-test-a &

./build/bin/amd64/vrndisd client \
    --server 127.0.0.1:7777 \
    --name test-client-b \
    --tap-name vrndis-test-b &

./build/bin/amd64/vrndisd client \
    --server 127.0.0.1:7777 \
    --name test-client-c \
    --tap-name vrndis-test-c &

sleep 2

echo ""
echo "=== 执行管理命令 ==="

# 1. 状态查询
echo "1. 服务器状态:"
./build/bin/amd64/vrndisd status --server 127.0.0.1:7777

echo ""
echo "2. 在线客户端列表:"
./build/bin/amd64/vrndisd clients --server 127.0.0.1:7777

echo ""
echo "3. 移除测试客户端 b:"
./build/bin/amd64/vrndisd client \
    --server 127.0.0.1:7777 \
    --client test-client-b \
    --action kick

echo ""
echo "4. 更新后状态:"
./build/bin/amd64/vrndisd status --server 127.0.0.1:7777

echo ""
echo "5. 更新后客户端列表:"
./build/bin/amd64/vrndisd clients --server 127.0.0.1:7777

# 显示统计
clients_count=$(./build/bin/amd64/vrndisd clients --server 127.0.0.1:7777 | grep -c "test-")
echo ""
echo "当前在线测试客户端数量: $clients_count"

echo ""
echo "管理命令示例执行完成！"
echo "脚本执行也会被自动清理。"

