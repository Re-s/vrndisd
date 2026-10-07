#!/usr/bin/env bash
# L5 真实网卡压测：netns 多客户端 + 连通性 + 丢包统计。
# 需要 root。用法: sudo bash tests/stress-real.sh [二进制路径] [端口]
set -u
BIN="$(readlink -f "${1:-./vrndisd}")"
PORT="${2:-7777}"
NS1=vstress1
NS2=vstress2
PASS=0; FAIL=0
SERVER_PID=""; C1_PID=""; C2_PID=""

check() {
  if eval "$2" >/dev/null 2>&1; then
    echo "  ✅ $1"; PASS=$((PASS+1))
  else
    echo "  ❌ $1"; FAIL=$((FAIL+1))
  fi
}

cleanup() {
  # 按 PID 精确清理（绝不使用 pkill -f，避免误杀自身会话）
  [ -n "$C1_PID" ] && kill -9 "$C1_PID" 2>/dev/null
  [ -n "$C2_PID" ] && kill -9 "$C2_PID" 2>/dev/null
  [ -n "$SERVER_PID" ] && kill -9 "$SERVER_PID" 2>/dev/null
  ip netns del "$NS1" 2>/dev/null
  ip netns del "$NS2" 2>/dev/null
  ip link del vth1 2>/dev/null
  ip link del vth2 2>/dev/null
}
trap cleanup EXIT

echo "== 启动服务器 =="
"$BIN" server --listen "0.0.0.0:$PORT" > /tmp/vstress-server.log 2>&1 &
SERVER_PID=$!
sleep 1
check "服务器网卡 vrndis0 存在" "ip link show vrndis0"

echo "== 准备 netns 客户端 =="
ip netns add "$NS1"; ip netns add "$NS2"
ip link add vth1 type veth peer name vtc1
ip link add vth2 type veth peer name vtc2
ip link set vtc1 netns "$NS1"; ip link set vtc2 netns "$NS2"
ip addr add 192.168.100.1/24 dev vth1; ip link set vth1 up
ip addr add 192.168.101.1/24 dev vth2; ip link set vth2 up
ip netns exec "$NS1" ip addr add 192.168.100.2/24 dev vtc1
ip netns exec "$NS1" ip link set vtc1 up; ip netns exec "$NS1" ip link set lo up
ip netns exec "$NS2" ip addr add 192.168.101.2/24 dev vtc2
ip netns exec "$NS2" ip link set vtc2 up; ip netns exec "$NS2" ip link set lo up

ip netns exec "$NS1" "$BIN" client --server 192.168.100.1:$PORT --tap vrndis-c0 --name c1 > /tmp/vstress-c1.log 2>&1 &
C1_PID=$!
sleep 1   # 保证 c1 先拿 10.77.0.2，c2 拿 .3（消除启动竞态）
ip netns exec "$NS2" "$BIN" client --server 192.168.101.1:$PORT --tap vrndis-c0 --name c2 > /tmp/vstress-c2.log 2>&1 &
C2_PID=$!
sleep 2

echo "== 连通性 =="
check "client1 拿到 10.77.0.2" "ip netns exec $NS1 ip -4 addr show vrndis-c0 | grep 'inet 10.77.0.2'"
check "client2 拿到 10.77.0.3" "ip netns exec $NS2 ip -4 addr show vrndis-c0 | grep 'inet 10.77.0.3'"
check "client1 -> 网关" "ip netns exec $NS1 ping -c 3 -W 2 10.77.0.1"
check "client1 -> client2" "ip netns exec $NS1 ping -c 3 -W 2 10.77.0.3"
check "client2 -> client1" "ip netns exec $NS2 ping -c 3 -W 2 10.77.0.2"

echo "== 丢包测试（ping 500 包，1ms 间隔）=="
LOSS=$(ip netns exec "$NS1" ping -c 500 -i 0.01 -W 2 -q 10.77.0.3 | grep "packet loss" | sed 's/.*[[:space:]]\([0-9.]*\)% packet loss.*/\1/')
echo "  丢包率: ${LOSS:-?}%"
if [ "${LOSS:-100}" = "0" ] || [ "${LOSS:-100}" = "0.0" ]; then PASS=$((PASS+1)); else FAIL=$((FAIL+1)); fi

echo "== 吞吐（如有 iperf3，UDP 5s @100M）=="
if command -v iperf3 >/dev/null; then
  ip netns exec "$NS2" iperf3 -s -D >/dev/null 2>&1
  sleep 0.5
  ip netns exec "$NS1" iperf3 -c 10.77.0.3 -u -b 100M -t 5 2>&1 | tail -4
else
  echo "  （未安装 iperf3，跳过）"
fi

echo "== 结果: PASS=$PASS FAIL=$FAIL =="
[ "$FAIL" -eq 0 ]
