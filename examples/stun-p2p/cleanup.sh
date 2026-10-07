#!/bin/bash
pkill -f stun-server
pkill -f vrndisd
iptables -t nat -D PREROUTING -p udp --dport 3478 -j ACCEPT 2>/dev/null || true
iptables -D INPUT -p udp --dport 3478 -j ACCEPT 2>/dev/null || true
echo "清理完成"
