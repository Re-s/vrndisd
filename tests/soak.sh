#!/bin/bash
# L6 长稳测试：服务器常驻 + 定时重连风暴 + 内存/线程监控。
#
# 本机生产服务（vrndisd.service, 7777, 10.77.0.0/24）不会受影响：
# 测试用独立端口 17777 + 独立网段 10.250.0.0/24 + 独立 tap 名。
#
# 用法：
#   sudo bash tests/soak.sh [vrndisd路径] [持续时长(秒，默认86400=24h)] [重连间隔(秒，默认30)]
#
# 输出：
#   /tmp/vrndis-soak/soak.log     — 测试日志
#   /tmp/vrndis-soak/mem.csv      — 资源采样（unix时间,RSS-KB,线程数,客户端数）
#   /tmp/vrndis-soak/report.txt   — 最终报告
set -euo pipefail

BIN="${1:-/root/vrndis-go/build/bin/amd64/vrndisd}"
DURATION="${2:-86400}"
RECONNECT_INTERVAL="${3:-30}"
OUTDIR="/tmp/vrndis-soak"
TAP_SVR="soak-srv0"

mkdir -p "$OUTDIR"
LOG="$OUTDIR/soak.log"
MEMCSV="$OUTDIR/mem.csv"
REPORT="$OUTDIR/report.txt"

echo "[soak] 二进制: $BIN  时长: ${DURATION}s  重连间隔: ${RECONNECT_INTERVAL}s" | tee "$LOG"

# ---- 1. 启动测试服务器（前台模式后台运行，独立端口/网段）----
"$BIN" server --listen 127.0.0.1:17777 --network 10.250.0.0/24 \
    --gateway 10.250.0.1 --tap "$TAP_SVR" \
    --management-socket /tmp/vrndis-soak/management.sock >>"$LOG" 2>&1 &
srv_pid=$!
trap '{
    kill "'"$srv_pid"'" 2>/dev/null || true
    wait "'"$srv_pid"'" 2>/dev/null || true
    ip link delete soak-c0 2>/dev/null || true
    echo "[soak] 已清理测试进程与网卡" >> "$LOG"
}' EXIT

sleep 2
if ! kill -0 "$srv_pid" 2>/dev/null; then
    echo "[soak] 服务器启动失败" | tee -a "$LOG"
    exit 1
fi
echo "[soak] 服务器已启动 pid=$srv_pid" >>"$LOG"

# ---- 2. 后台重连风暴：周期性连接/断开 ----
stop_storm=false
(
    while [ "$stop_storm" != "true" ]; do
        # 连接 → 保持一个周期 → 断开（模拟客户端 churn）
        timeout "$RECONNECT_INTERVAL" "$BIN" client --server 127.0.0.1:17777 \
            --tap soak-c0 --name soak-cli --reconnect=false >>"$LOG" 2>&1 || true
        ip link delete soak-c0 2>/dev/null || true
        sleep 1
    done
) &
storm_pid=$!

# ---- 3. 资源监控 ----
echo "timestamp,rss_kb,threads" > "$MEMCSV"
end_time=$(( $(date +%s) + DURATION ))
while [ "$(date +%s)" -lt "$end_time" ]; do
    sleep 60
    if kill -0 "$srv_pid" 2>/dev/null; then
        rss=$(ps -o rss= -p "$srv_pid" 2>/dev/null | tr -d ' ' || echo "0")
        nproc=$(ls "/proc/$srv_pid/task" 2>/dev/null | wc -l || echo "0")
        echo "$(date +%s),$rss,$nproc" >> "$MEMCSV"
    fi
done

stop_storm=true
kill "$storm_pid" 2>/dev/null || true
wait "$storm_pid" 2>/dev/null || true

# ---- 4. 生成报告 ----
{
    echo "===== vrndis L6 长稳测试报告 ====="
    echo "时间: $(date)"
    echo "二进制: $BIN"
    echo "时长: ${DURATION}s  重连间隔: ${RECONNECT_INTERVAL}s"
    echo ""
    echo "--- 内存/线程采样（每分钟）---"
    cat "$MEMCSV"
    echo ""
    # 内存增长判断
    first=$(sed -n '2p' "$MEMCSV" | cut -d, -f2)
    last=$(tail -1 "$MEMCSV" | cut -d, -f2)
    if [ -n "$first" ] && [ -n "$last" ] && [ "${first:-0}" -gt 0 ]; then
        growth_pct=$(( (last - first) * 100 / first ))
        echo ""
        echo "RSS 变化: ${first}KB -> ${last}KB （增长 ${growth_pct}%）"
        if [ "$growth_pct" -lt 5 ]; then
            echo "结论: PASS（内存增长 < 5%）"
        else
            echo "结论: WARN（内存增长 >= 5%，需检查泄漏）"
        fi
    fi
} | tee "$REPORT"

echo "[soak] 测试完成，报告: $REPORT" | tee -a "$LOG"
