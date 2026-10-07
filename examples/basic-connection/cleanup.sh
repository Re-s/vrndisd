#!/bin/bash
pkill -f vrndisd
rm -f /tmp/server.pid /tmp/client.pid
iptables -F 2>/dev/null || true
echo "清理完成"
