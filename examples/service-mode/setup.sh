#!/bin/bash

echo "=== VRNDIS-Go 服务管理模式示例 ==="
echo "此脚本将配置 VRNDIS-Go 为系统服务"

if [ "$EUID" -ne 0 ]; then 
    echo "请使用 sudo 运行此脚本"
    exit 1
fi

# 检查现有服务
if command -v systemctl >/dev/null 2>&1; then
    if systemctl is-active --quiet vrndis-server 2>/dev/null; then
        echo "服务已运行，停止当前服务..."
        systemctl stop vrndis-server
        sleep 2
    fi
fi

# 安装服务
echo "安装 vrndis 系统服务..."
./build/bin/vrndisd service install \
    --mode server \
    --network 10.77.0.0/24 \
    --gateway 10.77.0.1 \
    --listen 0.0.0.0:7777 \
    --name vrndis-server

# 启动服务
echo "启动服务..."
if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload
    systemctl enable vrndis-server
    systemctl start vrndis-server
    systemctl status vrndis-server
fi

# 等待服务就绪
sleep 2

echo ""
echo "=== 服务状态 ==="
if command -v systemctl >/dev/null 2>&1; then
    systemctl status vrndis-server --no-pager
else
    service vrndis-server status
fi

echo ""
echo "服务管理模式示例配置完成！"
echo "运行 cleanup.sh 卸载服务。"

