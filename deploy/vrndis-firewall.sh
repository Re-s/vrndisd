#!/bin/bash
# vrndis 隧道网络的服务器侧防火墙/NAT 规则（生产示例，幂等）。
# 用法：
#   ./vrndis-firewall.sh            # 应用（先清后建，重复执行安全）
#   ./vrndis-firewall.sh --remove   # 仅清除
# 配 systemd oneshot 服务开机自启：
#   cp vrndis-firewall.sh /usr/local/bin/
#   cp vrndis-firewall.service /etc/systemd/system/
#   systemctl enable --now vrndis-firewall.service
set -euo pipefail

NET="10.77.0.0/24"     # 与 server --network 一致
TAP="vrndis0"          # 与 server --tap 一致（默认 vrndis0）

purge() {
	while iptables -D FORWARD -i "$TAP" -j ACCEPT 2>/dev/null; do :; done
	while iptables -D FORWARD -o "$TAP" -j ACCEPT 2>/dev/null; do :; done
	while iptables -t nat -D POSTROUTING -s "$NET" -j MASQUERADE 2>/dev/null; do :; done
}

[ "${1:-}" = "--remove" ] && { purge; echo "vrndis firewall rules removed"; exit 0; }

purge
iptables -I FORWARD 1 -i "$TAP" -j ACCEPT
iptables -I FORWARD 1 -o "$TAP" -j ACCEPT
iptables -t nat -A POSTROUTING -s "$NET" -j MASQUERADE
echo "vrndis firewall rules ok"
