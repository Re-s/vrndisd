#!/bin/bash
echo "清理多客户端网络..."
pkill -f vrndisd
rm -f /tmp/vrndis_multi_server.pid
iptables -F 2>/dev/null || true
echo "清理完成"
