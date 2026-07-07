#!/usr/bin/env bash

set -uo pipefail

export PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH

TARGET_CPU_PERCENT=${1:-30}
DURATION=${2:-300}
THREADS=${3:-auto}
NICE_LEVEL=${4:-19}
DISABLE_CGROUP=${5:-false}
CUSTOM_PERIOD_MS=${6:-auto}

GROUP_NAME=${GROUP_NAME:-cpu_holder_v2_$$}
CGROUP_ENABLED=false
CGROUP_TYPE=""
CGROUP_ROOT=""
CGROUP_PATH=""
CGROUP_ATTACH_FILE=""
HOLDER_FILE=""
HOLDER_PID=""
MONITOR_PID=""
ABORT_REASON=""
CLEANED_UP=false

DUTY_PERIOD_MS=${DUTY_PERIOD_MS:-100}
BEST_EFFORT_MARGIN_PERCENT=${BEST_EFFORT_MARGIN_PERCENT:-0}
CPU_TARGET_MODE=${CPU_TARGET_MODE:-fill}
CGROUP_WORKER_MULTIPLIER=${CGROUP_WORKER_MULTIPLIER:-2}
MIN_CGROUP_WORKERS=${MIN_CGROUP_WORKERS:-4}
CPU_INNER_BATCH=${CPU_INNER_BATCH:-50000}

usage() {
    cat <<'EOF'
CPU 负载保持工具 v2（无外部压测工具）

用法:
  sudo ./cpu_load_limit_v2.sh [CPU%] [时长s] [线程数] [nice值] [禁用cgroup] [调度周期ms]

参数:
  CPU%        目标整机 CPU 占用率，默认 30
  时长s       运行持续时间，默认 300
  线程数      auto 或具体 worker 数，默认 auto
  nice值      -20 到 19，默认 19
  禁用cgroup  true/false，默认 false
  调度周期ms  cgroup CPU period，auto=架构自适应

环境变量:
  CPU_TARGET_MODE=fill             fill=补齐到目标占用率；add=额外增加 CPU% 负载
  DUTY_PERIOD_MS=100              无 cgroup 时 busy/sleep duty-cycle 周期
  BEST_EFFORT_MARGIN_PERCENT=0    无 cgroup 时额外降低 duty 的百分点
  CGROUP_WORKER_MULTIPLIER=2      cgroup auto worker 饱和倍数，过低可能达不到目标
  MIN_CGROUP_WORKERS=4            cgroup auto worker 最小值
  CPU_INNER_BATCH=50000           每次看时钟前的纯计算批量，越大加压越强但响应稍慢

说明:
  v2 不依赖外部压测程序。它使用轻量 Python 多进程 worker 消耗用户态 CPU。
  使用 nice 启动 worker，因此 top 中 CPU 时间会记入 ni。
  默认 fill 模式会补齐到目标 CPU 使用率，而不是额外叠加目标百分比。
  cgroup 可用时使用 CPU quota 做硬限制；不可用或禁用时继续运行并进入 best-effort duty-cycle 模式。
EOF
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
    usage
    exit 0
fi

log() {
    echo "[*] $*"
}

ok() {
    echo "[✓] $*"
}

warn() {
    echo "[!] $*" >&2
}

die() {
    echo "✗ $*" >&2
    exit 1
}

validate_int_range() {
    local name=$1
    local value=$2
    local min=$3
    local max=$4

    if ! [[ "$value" =~ ^-?[0-9]+$ ]]; then
        die "$name 必须是整数: $value"
    fi
    if [[ $value -lt $min || $value -gt $max ]]; then
        die "$name 必须在 $min 到 $max 之间: $value"
    fi
}

float_to_int() {
    local value=$1
    echo "${value%.*}"
}

detect_cpu_usage() {
    local usage="" idle_pct

    if command -v vmstat >/dev/null 2>&1; then
        idle_pct=$(vmstat 1 2 | tail -1 | awk '{print $15}')
        if [[ -n "$idle_pct" && "$idle_pct" =~ ^[0-9]+$ ]]; then
            usage=$((100 - idle_pct))
        fi
    fi

    if [[ -z "$usage" ]] && command -v mpstat >/dev/null 2>&1; then
        idle_pct=$(mpstat 1 1 | tail -1 | awk '{print $NF}')
        if [[ -n "$idle_pct" && "$idle_pct" =~ ^[0-9.]+$ ]]; then
            usage=$(awk "BEGIN {printf \"%.1f\", 100 - $idle_pct}")
        fi
    fi

    if [[ -z "$usage" ]] && command -v top >/dev/null 2>&1; then
        idle_pct=$(TERM=xterm COLUMNS=200 top -bn2 -d 0.5 2>/dev/null | grep -i "cpu" | grep -v "^top" | tail -1 | sed 's/.*[[:space:]]\([0-9.]*\)[[:space:]]*id.*/\1/')
        if [[ -n "$idle_pct" && "$idle_pct" =~ ^[0-9.]+$ ]]; then
            usage=$(awk "BEGIN {printf \"%.1f\", 100 - $idle_pct}")
        fi
    fi

    echo "$usage"
}

load_average() {
    if [[ -f /proc/loadavg ]]; then
        awk '{print $1}' /proc/loadavg
    elif command -v uptime >/dev/null 2>&1; then
        uptime | awk -F'load average:' '{print $2}' | awk -F',' '{print $1}' | xargs
    fi
}

write_sysfs() {
    local value=$1
    local file=$2

    printf '%s\n' "$value" > "$file" 2>/dev/null
}

enable_cgroup() {
    local quota_us=$1
    local period_us=$2

    if [[ "$DISABLE_CGROUP" == "true" ]]; then
        warn "cgroup 已被显式禁用；当前为 best-effort duty-cycle 模式，无法提供 CPU 硬限制"
        return
    fi

    if [[ $EUID -ne 0 ]]; then
        warn "非 root 运行，无法配置 cgroup；当前为 best-effort duty-cycle 模式"
        return
    fi

    if [[ -f /sys/fs/cgroup/cgroup.controllers ]]; then
        CGROUP_TYPE="v2"
        CGROUP_ROOT="/sys/fs/cgroup"
        CGROUP_PATH="$CGROUP_ROOT/$GROUP_NAME"
        mkdir -p "$CGROUP_PATH" || die "无法创建 cgroup: $CGROUP_PATH"
        CGROUP_ATTACH_FILE="$CGROUP_PATH/cgroup.procs"

        if [[ -w "$CGROUP_ROOT/cgroup.subtree_control" ]]; then
            write_sysfs "+cpu" "$CGROUP_ROOT/cgroup.subtree_control" || true
        fi

        write_sysfs "$quota_us $period_us" "$CGROUP_PATH/cpu.max" || die "无法设置 cgroup v2 cpu.max"
        CGROUP_ENABLED=true
        ok "已配置 cgroup v2 CPU 限制: ${quota_us}us / ${period_us}us"
    elif [[ -d /sys/fs/cgroup/cpu ]]; then
        CGROUP_TYPE="v1"
        CGROUP_ROOT="/sys/fs/cgroup/cpu"
        CGROUP_PATH="$CGROUP_ROOT/$GROUP_NAME"
        mkdir -p "$CGROUP_PATH" || die "无法创建 cgroup: $CGROUP_PATH"
        CGROUP_ATTACH_FILE="$CGROUP_PATH/tasks"

        write_sysfs "$period_us" "$CGROUP_PATH/cpu.cfs_period_us" || die "无法设置 cpu.cfs_period_us"
        write_sysfs "$quota_us" "$CGROUP_PATH/cpu.cfs_quota_us" || die "无法设置 cpu.cfs_quota_us"
        CGROUP_ENABLED=true
        ok "已配置 cgroup v1 CPU 限制: quota=${quota_us}us period=${period_us}us"
    else
        warn "未检测到可用 cgroup；当前为 best-effort duty-cycle 模式"
    fi
}

create_holder_file() {
    HOLDER_FILE=$(mktemp /tmp/cpu_holder_v2.XXXXXX.py) || die "无法创建 holder 临时文件"
    cat > "$HOLDER_FILE" <<'PY'
import multiprocessing as mp
import os
import signal
import sys
import time

running = mp.Event()
running.set()

def stop(_signum, _frame):
    running.clear()

def worker(index, mode, duty_percent, duty_period_ms, duration, inner_batch):
    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)

    x = (index + 1) * 2654435761
    deadline = time.monotonic() + duration
    period = max(duty_period_ms, 10) / 1000.0
    busy = period * max(0.0, min(100.0, duty_percent)) / 100.0

    while running.is_set() and time.monotonic() < deadline:
        if mode == "cgroup":
            end = min(time.monotonic() + 0.2, deadline)
            while running.is_set() and time.monotonic() < end:
                for _ in range(inner_batch):
                    x = ((x * 1103515245) + 12345) & 0xFFFFFFFF
        else:
            start = time.monotonic()
            end = min(start + busy, deadline)
            while running.is_set() and time.monotonic() < end:
                for _ in range(inner_batch):
                    x = ((x * 1103515245) + 12345) & 0xFFFFFFFF
            sleep_for = period - (time.monotonic() - start)
            if sleep_for > 0:
                time.sleep(sleep_for)

    if x == 0x12345678:
        print("", end="")

def main():
    workers = int(sys.argv[1])
    duration = int(sys.argv[2])
    mode = sys.argv[3]
    duty_percent = float(sys.argv[4])
    duty_period_ms = int(sys.argv[5])
    inner_batch = int(sys.argv[6])

    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)

    procs = []
    for i in range(workers):
        proc = mp.Process(target=worker, args=(i, mode, duty_percent, duty_period_ms, duration, inner_batch))
        proc.start()
        procs.append(proc)

    deadline = time.monotonic() + duration
    try:
        while running.is_set() and time.monotonic() < deadline:
            alive = [p for p in procs if p.is_alive()]
            if not alive:
                break
            time.sleep(0.5)
    finally:
        running.clear()
        for proc in procs:
            if proc.is_alive():
                proc.terminate()
        for proc in procs:
            proc.join(timeout=2)
            if proc.is_alive():
                proc.kill()

if __name__ == "__main__":
    main()
PY
}

cleanup() {
    if [[ "$CLEANED_UP" == "true" ]]; then
        return
    fi
    CLEANED_UP=true

    if [[ -n "$ABORT_REASON" ]]; then
        warn "$ABORT_REASON"
    fi

    if [[ -n "$HOLDER_PID" ]]; then
        log "正在停止 CPU holder..."
        kill "$HOLDER_PID" 2>/dev/null || true
        wait "$HOLDER_PID" 2>/dev/null || true
    fi

    if [[ -n "$MONITOR_PID" ]]; then
        kill "$MONITOR_PID" 2>/dev/null || true
        wait "$MONITOR_PID" 2>/dev/null || true
    fi

    if [[ -n "$HOLDER_FILE" && -f "$HOLDER_FILE" ]]; then
        rm -f "$HOLDER_FILE"
    fi

    if [[ -n "$CGROUP_PATH" && -d "$CGROUP_PATH" ]]; then
        log "清理 cgroup: $CGROUP_PATH"
        rmdir "$CGROUP_PATH" 2>/dev/null || true
    fi
}

trap cleanup EXIT
trap 'ABORT_REASON="收到 SIGINT，停止 CPU holder 并退出"; exit 130' INT
trap 'ABORT_REASON="收到 SIGTERM，停止 CPU holder 并退出"; exit 143' TERM

start_holder() {
    local workers=$1
    local mode=$2
    local duty=$3

    (
        if [[ "$CGROUP_ENABLED" == "true" ]]; then
            echo "$BASHPID" > "$CGROUP_ATTACH_FILE" || exit 97
        fi
        exec nice -n "$NICE_LEVEL" python3 "$HOLDER_FILE" "$workers" "$DURATION" "$mode" "$duty" "$DUTY_PERIOD_MS" "$CPU_INNER_BATCH"
    ) &
    HOLDER_PID=$!
    log "CPU holder 已启动: pid=$HOLDER_PID workers=$workers mode=$mode duty=${duty}%"
}

cgroup_usage_seconds() {
    local usage

    if [[ "$CGROUP_TYPE" == "v2" && -f "$CGROUP_PATH/cpu.stat" ]]; then
        usage=$(awk '$1 == "usage_usec" {print $2; exit}' "$CGROUP_PATH/cpu.stat")
        if [[ -n "$usage" ]]; then
            echo $((usage / 1000000))
            return
        fi
    elif [[ "$CGROUP_TYPE" == "v1" && -f "$CGROUP_PATH/cpuacct.usage" ]]; then
        usage=$(cat "$CGROUP_PATH/cpuacct.usage" 2>/dev/null || echo "")
        if [[ -n "$usage" ]]; then
            echo $((usage / 1000000000))
            return
        fi
    fi

    echo ""
}

recommended_worker_count() {
    local cores=$1
    local percent=$2
    local count

    count=$(((cores * percent + 99) / 100))
    count=$((count * CGROUP_WORKER_MULTIPLIER))
    if [[ $count -lt $MIN_CGROUP_WORKERS ]]; then
        count=$MIN_CGROUP_WORKERS
    fi
    if [[ $count -lt 1 ]]; then
        count=1
    fi
    if [[ $count -gt $cores ]]; then
        count=$cores
    fi
    echo "$count"
}

monitor_cpu() {
    local count=0
    local max_samples=$((DURATION / 5))
    local usage load timestamp output cg_usage

    if [[ $max_samples -lt 1 ]]; then
        max_samples=1
    fi

    while [[ $count -lt $max_samples ]]; do
        if [[ -n "$HOLDER_PID" ]] && ! kill -0 "$HOLDER_PID" 2>/dev/null; then
            echo "[*] CPU holder 已结束，停止监控"
            break
        fi

        usage=$(detect_cpu_usage)
        load=$(load_average)
        timestamp=$(date '+%H:%M:%S')
        output="[$timestamp]"

        if [[ -n "$usage" ]]; then
            output="$output 系统 CPU: ${usage}%"
        else
            output="$output 系统 CPU: 检测中..."
        fi
        if [[ -n "$load" ]]; then
            output="$output | 系统负载: ${load}"
        fi
        output="$output | 目标: ${TARGET_CPU_PERCENT}%"
        echo "$output"

        if [[ "$CGROUP_ENABLED" == "true" && $((count % 6)) -eq 0 ]]; then
            cg_usage=$(cgroup_usage_seconds)
            if [[ -n "$cg_usage" ]]; then
                echo "    └─ cgroup 累计 CPU 时间: ${cg_usage}s"
            fi
        fi

        sleep 5
        count=$((count + 1))
    done
}

validate_int_range "目标 CPU 占用率" "$TARGET_CPU_PERCENT" 1 100
validate_int_range "运行时长" "$DURATION" 1 86400
validate_int_range "nice 值" "$NICE_LEVEL" -20 19
validate_int_range "duty 周期" "$DUTY_PERIOD_MS" 10 10000
validate_int_range "best-effort 降低百分点" "$BEST_EFFORT_MARGIN_PERCENT" 0 99
validate_int_range "cgroup worker 饱和倍数" "$CGROUP_WORKER_MULTIPLIER" 1 16
validate_int_range "cgroup worker 最小值" "$MIN_CGROUP_WORKERS" 1 4096
validate_int_range "CPU 内层计算批量" "$CPU_INNER_BATCH" 100 10000000
if [[ "$CPU_TARGET_MODE" != "fill" && "$CPU_TARGET_MODE" != "add" ]]; then
    die "CPU_TARGET_MODE 必须是 fill 或 add: $CPU_TARGET_MODE"
fi

if [[ "$(uname -s)" != "Linux" ]]; then
    die "v2 版本当前仅支持 Linux；cgroup CPU quota 与 /proc CPU 监控依赖 Linux 接口"
fi
command -v python3 >/dev/null 2>&1 || die "缺少 python3，无法启动 CPU holder"

ARCH=$(uname -m)
CPU_CORES=$(nproc 2>/dev/null || echo "1")

if [[ "$CUSTOM_PERIOD_MS" != "auto" ]]; then
    validate_int_range "调度周期毫秒" "$CUSTOM_PERIOD_MS" 1 10000
    PERIOD_US=$((CUSTOM_PERIOD_MS * 1000))
elif [[ "$ARCH" =~ ^(aarch64|arm64|armv7l|armv8)$ ]]; then
    PERIOD_US=1000000
else
    PERIOD_US=100000
fi

THREADS_AUTO=false
if [[ "$THREADS" == "auto" ]]; then
    THREADS_AUTO=true
    THREADS="$CPU_CORES"
else
    validate_int_range "线程数" "$THREADS" 1 4096
fi

CURRENT_CPU_USAGE=$(detect_cpu_usage)
LOAD_AVERAGE=$(load_average)
NEED_LOAD=true
CURRENT_CPU_INT=0
if [[ -n "$CURRENT_CPU_USAGE" ]]; then
    CURRENT_CPU_INT=$(float_to_int "$CURRENT_CPU_USAGE")
fi

if [[ "$CPU_TARGET_MODE" == "fill" ]]; then
    if [[ -n "$CURRENT_CPU_USAGE" ]]; then
        NEEDED_CPU_PERCENT=$((TARGET_CPU_PERCENT - CURRENT_CPU_INT))
    else
        NEEDED_CPU_PERCENT=$TARGET_CPU_PERCENT
    fi
    if [[ $NEEDED_CPU_PERCENT -le 0 ]]; then
        NEED_LOAD=false
    fi
else
    NEEDED_CPU_PERCENT=$TARGET_CPU_PERCENT
fi

if [[ "$NEED_LOAD" == "true" && $NEEDED_CPU_PERCENT -lt 1 ]]; then
    NEEDED_CPU_PERCENT=1
fi

QUOTA_US=$((CPU_CORES * NEEDED_CPU_PERCENT * PERIOD_US / 100))
if [[ $QUOTA_US -lt 1000 ]]; then
    QUOTA_US=1000
fi

echo "=========================================="
echo "[*] CPU 负载保持工具 v2 - 无外部压测工具"
echo "=========================================="
log "系统架构: $ARCH"
log "逻辑核心数: $CPU_CORES"
log "目标 CPU 使用率: ${TARGET_CPU_PERCENT}%"
log "目标模式: ${CPU_TARGET_MODE}"
log "运行时长: ${DURATION}s"
log "worker 参数: ${3:-auto}"
log "nice 优先级: $NICE_LEVEL"
if [[ -n "$CURRENT_CPU_USAGE" ]]; then
    log "当前 CPU 使用率: ${CURRENT_CPU_USAGE}%"
else
    warn "无法检测当前 CPU 使用率，将按目标启动 holder"
fi
if [[ -n "$LOAD_AVERAGE" ]]; then
    log "1 分钟负载平均: $LOAD_AVERAGE"
fi

if [[ "$NEED_LOAD" == "false" ]]; then
    ok "当前 CPU 使用率已达到目标，无需额外占用；仅监控 ${DURATION}s"
    monitor_cpu &
    MONITOR_PID=$!
    wait "$MONITOR_PID"
    exit 0
fi

if [[ "$CPU_TARGET_MODE" == "fill" ]]; then
    log "需要补齐 CPU: ${NEEDED_CPU_PERCENT}%（当前 ${CURRENT_CPU_USAGE:-未知}%，目标 ${TARGET_CPU_PERCENT}%）"
else
    log "需要额外增加 CPU: ${NEEDED_CPU_PERCENT}%（add 模式）"
fi

enable_cgroup "$QUOTA_US" "$PERIOD_US"
create_holder_file

MODE="best-effort"
DUTY_PERCENT=$((NEEDED_CPU_PERCENT - BEST_EFFORT_MARGIN_PERCENT))
if [[ $DUTY_PERCENT -lt 1 ]]; then
    DUTY_PERCENT=1
fi
EFFECTIVE_THREADS="$THREADS"

if [[ "$CGROUP_ENABLED" == "true" ]]; then
    MODE="cgroup"
    DUTY_PERCENT=100
    RECOMMENDED_THREADS=$(recommended_worker_count "$CPU_CORES" "$NEEDED_CPU_PERCENT")
    if [[ "$THREADS_AUTO" == "true" ]]; then
        EFFECTIVE_THREADS="$RECOMMENDED_THREADS"
    elif [[ "$THREADS" -gt "$RECOMMENDED_THREADS" ]]; then
        warn "当前显式 worker 数 ${THREADS} 高于 holder 预算 ${NEEDED_CPU_PERCENT}% 推荐值 ${RECOMMENDED_THREADS}；CPU 仍受 cgroup 限制，但 load average 可能偏高"
    fi
    log "运行模式: cgroup hard-limit（worker 持续忙跑，由 cgroup 限制总 CPU）"
    log "cgroup CPU quota: ${QUOTA_US}us / ${PERIOD_US}us（holder 预算 ${NEEDED_CPU_PERCENT}% 整机 CPU）"
    log "实际 worker 数: ${EFFECTIVE_THREADS}（auto 推荐值已按 ${CGROUP_WORKER_MULTIPLIER}x 饱和倍数计算）"
else
    log "运行模式: best-effort duty-cycle（无 CPU 硬限制）"
    log "duty-cycle: busy ${DUTY_PERCENT}% / period ${DUTY_PERIOD_MS}ms"
    log "实际 worker 数: ${EFFECTIVE_THREADS}"
fi

if [[ "$EFFECTIVE_THREADS" -lt "$CPU_CORES" && "$CGROUP_ENABLED" != "true" ]]; then
    warn "当前 worker 数 ${EFFECTIVE_THREADS} 小于逻辑核心数 ${CPU_CORES}；无 cgroup 时可能无法达到整机目标 CPU"
fi

start_holder "$EFFECTIVE_THREADS" "$MODE" "$DUTY_PERCENT"
monitor_cpu &
MONITOR_PID=$!

wait "$HOLDER_PID"
HOLDER_EXIT=$?

kill "$MONITOR_PID" 2>/dev/null || true
wait "$MONITOR_PID" 2>/dev/null || true

echo "=========================================="
if [[ $HOLDER_EXIT -eq 0 ]]; then
    ok "CPU 负载保持完成"
else
    echo "[✗] CPU holder 异常退出，退出码: $HOLDER_EXIT"
fi

if [[ "$CGROUP_ENABLED" == "true" ]]; then
    TOTAL_CPU_TIME_SEC=$(cgroup_usage_seconds)
    if [[ -n "$TOTAL_CPU_TIME_SEC" ]]; then
        EXPECTED_MAX_TIME=$((CPU_CORES * NEEDED_CPU_PERCENT * DURATION / 100))
        log "cgroup 统计信息:"
        echo "    ├─ 总 CPU 时间: ${TOTAL_CPU_TIME_SEC}s"
        echo "    ├─ 预期最大时间: ${EXPECTED_MAX_TIME}s (${CPU_CORES}核 × holder ${NEEDED_CPU_PERCENT}% × ${DURATION}s)"
        echo "    └─ 限制效果: $( [[ $TOTAL_CPU_TIME_SEC -le $((EXPECTED_MAX_TIME + CPU_CORES)) ]] && echo "✓ 有效" || echo "✗ 可能超限" )"
    fi
fi

echo "=========================================="
