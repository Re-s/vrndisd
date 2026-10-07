#!/usr/bin/env bash
# 崩溃恢复测试：反复 kill -9 客户端并重连，验证服务器租约回收与地址复用。
# 需要 root。用法: sudo bash tests/reconnect-loop.sh [二进制] [端口] [循环次数]
set -u
BIN="$(readlink -f "${1:-./vrndisd}")"
PORT="${2:-7777}"
LOOPS="${3:-50}"
NS=vreconn
OK=0; BAD=0
SERVER_PID=""; CLIENT_PID=""

cleanup() {
  [ -n "$CLIENT_PID" ] && kill -9 "$CLIENT_PID" 2>/dev/null
  [ -n "$SERVER_PID" ] && kill -9 "$SERVER_PID" 2>/dev/null
  ip netns del "$NS" 2>/dev/null
  ip link del vthr 2>/dev/null
}
trap cleanup EXIT

"$BIN" server --listen "0.0.0.0:$PORT" > /tmp/vreconn-server.log 2>&1 &
SERVER_PID=$!
sleep 1
ip netns add "$NS"
ip link add vthr type veth peer name vtcr
ip link set vtcr netns "$NS"
ip addr add 192.168.100.1/24 dev vthr; ip link set vthr up
ip netns exec "$NS" ip addr add 192.168.100.2/24 dev vtcr
ip netns exec "$NS" ip link set vtcr up; ip netns exec "$NS" ip link set lo up

for i in $(seq 1 "$LOOPS"); do
  ip netns exec "$NS" "$BIN" client --server 192.168.100.1:$PORT --tap vrndis-c0 --name rc$i > /tmp/vreconn-c.log 2>&1 &
  CLIENT_PID=$!
  sleep 0.8
  if ip netns exec "$NS" ping -c 1 -W 2 10.77.0.1 >/dev/null 2>&1; then
    OK=$((OK+1))
  else
    BAD=$((BAD+1)); echo "  ❌ 第 $i 次重连失败"
  fi
  kill -9 "$CLIENT_PID" 2>/dev/null   # 模拟客户端异常断电
  wait "$CLIENT_PID" 2>/dev/null
  CLIENT_PID=""
  sleep 0.3
done
echo "== 重连成功 $OK / $((OK+BAD)) =="
grep -c "回收" /tmp/vreconn-server.log | sed 's/^/服务器回收租约次数: /'
[ "$BAD" -eq 0 ]
