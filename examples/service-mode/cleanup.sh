#!/bin/bash
echo "卸载 VRNDIS-Go 服务..."
./build/bin/vrndisd service uninstall
if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload
    systemctl disable vrndis-server 2>/dev/null || true
    systemctl stop vrndis-server 2>/dev/null || true
fi
echo "服务卸载完成"
