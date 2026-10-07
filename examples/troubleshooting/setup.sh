#!/bin/bash

echo "=== VRNDIS-Go 故障排查示例 ==="
echo "这是一个纯粹的示例展示，用于演示故障排查流程，不会实际启动服务。"
echo ""

# 展示诊断脚本
cat << 'DIAGNOSTIC'
#!/bin/bash
# vrndis_diagnostic.sh - VRNDIS-Go 诊断脚本

echo "=== VRNDIS-Go 诊断脚本 ==="
echo "运行时间: $(date)"
echo ""

# 1. 服务状态检查
echo "1. 服务状态检查..."
if command -v systemctl >/dev/null 2>&1; then
    systemctl status vrndis-server --no-pager 2>/dev/null | head -20 || echo "服务未运行"
else
    service vrndis-server status 2>/dev/null || echo "服务未运行"
fi
echo ""

# 2. 端口检查
echo "2. 端口检查..."
ss -tlnp | grep 7777 || echo "端口 7777 未被占用"
echo ""

# 3. 日志检查
echo "3. 日志检查..."
if command -v journalctl >/dev/null 2>&1; then
    journalctl -u vrndis-server -n 20 --no-pager || echo "无日志"
else
    echo "未找到 journalctl"
fi
echo ""

# 4. 网络连通性
echo "4. 网络连通性测试..."
nc -vz 127.0.0.1 7777 && echo "TCP 7777 端口可连接" || echo "端口连接失败"
echo ""

# 5. 内存和 CPU 使用
echo "5. 资源使用..."
ps aux | grep vrndisd || echo "无 vrndisd 进程"
echo ""

# 6. TAP 设备
echo "6. TAP 设备检查..."
ip link show | grep -E "vrndis|tap" || echo "VRNDIS TAP 设备未找到"
echo ""

# 7. 版本信息
echo "7. 版本信息..."
./build/bin/vrndisd version 2>/dev/null || echo "无法获取版本"
echo ""

# 8. 客户端连接测试
echo "8. 客户端连接测试..."
./build/bin/vrndisd status --server 127.0.0.1:7777 2>/dev/null || echo "状态查询失败"
echo ""

echo "诊断完成！"
DIAGNOSTIC

chmod +x vrndis_diagnostic.sh

echo "✅ 已生成诊断脚本：vrndis_diagnostic.sh"
echo ""
echo "使用此脚本进行故障排查。"
echo ""
echo "示例故障场景演练："
echo "1. 假设服务器无法启动"
echo "2. 假设客户端连接超时"
echo "3. 假设 TAP 设备权限不足"
echo ""
echo "请阅读 diagnostic-README.md 获取详细解决方案。"
echo ""
echo "故障排查示例完成！"
echo ""

