#!/bin/sh
# vrndis-go Docker 入口脚本
# 负责按环境变量启动 stun-server 和/或 vrndisd。
#
# 用法:
#   docker run ... vrndis-go:arm64                      # 仅启动 vrndisd server
#   docker run -e ROLE=stun ... vrndis-go:arm64         # 仅启动 stun-server
#   docker run -e ROLE=all   ... vrndis-go:arm64        # 同时启动 stun-server + vrndisd
#
# 常用环境变量:
#   ROLE          all | stun | server   (默认 server)
#   LISTEN        服务端监听地址         (默认 0.0.0.0:7777)
#   NETWORK       虚拟网段 CIDR         (默认 10.77.0.0/24)
#   GATEWAY       网关地址             (默认 10.77.0.1)
#   DNS           逗号分隔 DNS         (默认 8.8.8.8,1.1.1.1)
#   TAP           服务器网卡名         (默认 vrndis0)
#   MTU           网卡 MTU             (默认 1500)
#   STUN_LISTEN   STUN 监听地址         (默认 0.0.0.0:3478)

set -e

ROLE=${ROLE:-server}
LISTEN=${LISTEN:-0.0.0.0:7777}
NETWORK=${NETWORK:-10.77.0.0/24}
GATEWAY=${GATEWAY:-10.77.0.1}
DNS=${DNS:-8.8.8.8,1.1.1.1}
TAP=${TAP:-vrndis0}
MTU=${MTU:-1500}
STUN_LISTEN=${STUN_LISTEN:-0.0.0.0:3478}

# 后台启动 stun-server (ROLE=all 或 stun)
start_stun() {
  if [ "$ROLE" = "all" ] || [ "$ROLE" = "stun" ]; then
    echo "[entrypoint] 启动 stun-server (${STUN_LISTEN}) ..."
    /stun-server --listen "$STUN_LISTEN" &
    STUN_PID=$!
  fi
}

# 前台启动 vrndisd server (ROLE=all 或 server)
start_vrndisd() {
  if [ "$ROLE" = "all" ] || [ "$ROLE" = "server" ]; then
    echo "[entrypoint] 启动 vrndisd server (${LISTEN}, net=${NETWORK}) ..."
    exec /vrndisd server \
      --listen "$LISTEN" \
      --network "$NETWORK" \
      --gateway "$GATEWAY" \
      --dns "$DNS" \
      --tap "$TAP" \
      --mtu "$MTU"
  fi
}

# ROLE=all 时 stun 后台 + vrndisd 前台；ROLE=stun 时仅 stun 前台
case "$ROLE" in
  all)
    start_stun
    start_vrndisd   # exec，保持 PID 1
    ;;
  stun)
    echo "[entrypoint] 启动 stun-server (${STUN_LISTEN}) ..."
    exec /stun-server --listen "$STUN_LISTEN"
    ;;
  server|*)
    start_vrndisd
    ;;
esac
