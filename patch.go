#!/usr/bin/env bash

set -uo pipefail

export PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH

TARGET_MEMORY_PERCENT=${1:-30}
DURATION=${2:-600}
NICE_LEVEL=${3:-19}
DISABLE_CGROUP=${4:-false}
MEMORY_SIZE=${5:-auto}

GROUP_NAME=${GROUP_NAME:-memory_holder_v2_$$}
HOLDER_CHUNK_MB=${HOLDER_CHUNK_MB:-64}
TOUCH_DELAY_MS=${TOUCH_DELAY_MS:-50}
MONITOR_INTERVAL=${MONITOR_INTERVAL:-5}
STRICT_SWAP=${STRICT_SWAP:-false}
NODE_RESERVE_PERCENT=${NODE_RESERVE_PERCENT:-10}
NODE_ALLOC_RATIO_PERCENT=${NODE_ALLOC_RATIO_PERCENT:-50}
MIN_NODE_RESERVE_MB=${MIN_NODE_RESERVE_MB:-auto}
SAFETY_OVERHEAD_MB=${SAFETY_OVERHEAD_MB:-128}
DISABLE_NUMA_SAFETY=${DISABLE_NUMA_SAFETY:-false}
NUMA_POLICY=${NUMA_POLICY:-interleave}
PSI_FULL_AVG10_ABORT=${PSI_FULL_AVG10_ABORT:-5.00}
PRESSURE_ABORT_SAMPLES=${PRESSURE_ABORT_SAMPLES:-3}
SWAP_ABORT_DELTA_MB=${SWAP_ABORT_DELTA_MB:-64}
GLOBAL_SWAP_ABORT=${GLOBAL_SWAP_ABORT:-true}
MEMORY_HIGH_PERCENT=${MEMORY_HIGH_PERCENT:-0}

CGROUP_ENABLED=false
CGROUP_TYPE=""
CGROUP_PATH=""
CGROUP_ATTACH_FILE=""
HOLDER_FILE=""
ABORT_REASON=""
CLEANED_UP=false
HOLDER_PIDS=()

usage() {
    cat <<'EOF'
内存占用保持工具 v2（无外部压测工具）

用法:
  sudo ./memory_usage_limit_v2.sh [内存%] [时长s] [nice值] [禁用cgroup] [内存大小]

参数:
  内存%       目标整机内存占用率，默认 30
  时长s       保持时间，默认 600
  nice值      -20 到 19，默认 19
  禁用cgroup  true/false，默认 false
  内存大小    auto 或具体值，例如 1024M、1G，默认 auto

安全环境变量:
  STRICT_SWAP=false             默认兼容运行；设为 true 时无法保证 swap 隔离则拒绝执行
  NODE_RESERVE_PERCENT=10       每个 NUMA node 至少保留的百分比
  MIN_NODE_RESERVE_MB=auto      每个 NUMA node 至少保留的 MB
  NODE_ALLOC_RATIO_PERCENT=50   最多使用全局可用内存的比例；保留旧变量名以兼容
  HOLDER_CHUNK_MB=64            holder 分块申请大小
  TOUCH_DELAY_MS=50             每个分块触页后的暂停时间
  SWAP_ABORT_DELTA_MB=64        SwapFree 连续下降超过该值则主动退出
  GLOBAL_SWAP_ABORT=true        无 cgroup 或 swap.current 不可用时用全局 swap 指标止损
  PSI_FULL_AVG10_ABORT=5.00     memory pressure full avg10 连续超过该值则主动退出
  PRESSURE_ABORT_SAMPLES=3      swap/PSI 连续异常多少次后退出
  MEMORY_HIGH_PERCENT=0         默认不设置 memory.high；1-100 时按 memory.max 百分比设置
  NUMA_POLICY=interleave        多 NUMA node 且有 numactl 时使用 interleave；可设 none 跳过
  DISABLE_NUMA_SAFETY=false     兼容旧变量；设 true 时等同于 NUMA_POLICY=none

说明:
  v2 只用轻量 Python holder 申请匿名内存、触页一次后睡眠保持。
  Linux 上默认尝试使用 cgroup 限制 holder 内存；cgroup 不可用时继续运行并打印风险警告。
  没有开启 NUMA 或未安装 numactl 时，会跳过 NUMA 绑定并正常占用内存。
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

min_int() {
    if [[ $1 -lt $2 ]]; then
        echo "$1"
    else
        echo "$2"
    fi
}

max_int() {
    if [[ $1 -gt $2 ]]; then
        echo "$1"
    else
        echo "$2"
    fi
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

parse_size_mb() {
    local raw=$1
    local number unit

    if [[ "$raw" =~ ^([0-9]+)([GgMm]?)$ ]]; then
        number=${BASH_REMATCH[1]}
        unit=${BASH_REMATCH[2]}
        case "$unit" in
            G|g) echo $((number * 1024)) ;;
            M|m|"") echo "$number" ;;
        esac
    else
        return 1
    fi
}

meminfo_kb() {
    local key=$1
    awk -v key="$key" '$1 == key ":" {print $2; exit}' /proc/meminfo
}

available_memory_mb() {
    local kb
    kb=$(meminfo_kb "$MEM_AVAILABLE_FIELD")
    echo $((kb / 1024))
}

swap_free_mb() {
    local kb
    kb=$(meminfo_kb SwapFree)
    echo $((kb / 1024))
}

vmstat_value() {
    local key=$1
    awk -v key="$key" '$1 == key {print $2; exit}' /proc/vmstat 2>/dev/null || echo 0
}

memory_psi_full_avg10() {
    awk '
        /^full/ {
            for (i = 1; i <= NF; i++) {
                if ($i ~ /^avg10=/) {
                    sub("avg10=", "", $i)
                    print $i
                    exit
                }
            }
        }
    ' /proc/pressure/memory 2>/dev/null || echo 0
}

float_gt() {
    awk -v left="$1" -v right="$2" 'BEGIN { exit !(left > right) }'
}

read_node_meminfo_mb() {
    local node=$1
    local key=$2
    local file="/sys/devices/system/node/node${node}/meminfo"

    awk -v key="$key" '$3 == key ":" {print int($4 / 1024); exit}' "$file"
}

node_reserve_mb() {
    local total_mb=$1
    local reserve floor cap

    if [[ "$MIN_NODE_RESERVE_MB" != "auto" ]]; then
        echo "$MIN_NODE_RESERVE_MB"
        return
    fi

    reserve=$((total_mb * NODE_RESERVE_PERCENT / 100))
    if [[ $total_mb -ge 16384 ]]; then
        floor=4096
    else
        floor=512
    fi

    reserve=$(max_int "$reserve" "$floor")
    cap=$((total_mb / 2))
    if [[ $reserve -gt $cap ]]; then
        reserve=$cap
    fi
    echo "$reserve"
}

create_holder_file() {
    HOLDER_FILE=$(mktemp /tmp/memory_holder_v2.XXXXXX.py) || die "无法创建 holder 临时文件"
    cat > "$HOLDER_FILE" <<'PY'
import mmap
import os
import signal
import sys
import time

running = True

def stop(_signum, _frame):
    global running
    running = False

signal.signal(signal.SIGTERM, stop)
signal.signal(signal.SIGINT, stop)

mb = int(sys.argv[1])
duration = int(sys.argv[2])
chunk_mb = int(sys.argv[3])
touch_delay_ms = int(sys.argv[4])
node_label = sys.argv[5]

page_size = os.sysconf("SC_PAGE_SIZE")
chunk_bytes = max(1, chunk_mb) * 1024 * 1024
target_bytes = mb * 1024 * 1024
allocated = []
allocated_bytes = 0
deadline = time.monotonic() + duration

try:
    while allocated_bytes < target_bytes and running:
        size = min(chunk_bytes, target_bytes - allocated_bytes)
        area = mmap.mmap(
            -1,
            size,
            flags=mmap.MAP_PRIVATE | mmap.MAP_ANONYMOUS,
            prot=mmap.PROT_READ | mmap.PROT_WRITE,
        )

        for offset in range(0, size, page_size):
            area[offset:offset + 1] = b"\1"

        allocated.append(area)
        allocated_bytes += size
        print(
            f"holder[{node_label}] allocated {allocated_bytes // 1024 // 1024}/{mb} MB",
            flush=True,
        )

        if touch_delay_ms > 0:
            time.sleep(touch_delay_ms / 1000.0)

    while running and time.monotonic() < deadline:
        time.sleep(1)
finally:
    allocated.clear()
PY
}

cleanup() {
    local pid

    if [[ "$CLEANED_UP" == "true" ]]; then
        return
    fi
    CLEANED_UP=true

    if [[ -n "$ABORT_REASON" ]]; then
        warn "$ABORT_REASON"
    fi

    if [[ ${#HOLDER_PIDS[@]} -gt 0 ]]; then
        log "正在停止 holder 进程..."
        for pid in "${HOLDER_PIDS[@]}"; do
            kill "$pid" 2>/dev/null || true
        done
        for pid in "${HOLDER_PIDS[@]}"; do
            wait "$pid" 2>/dev/null || true
        done
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
trap 'ABORT_REASON="收到 SIGINT，停止 holder 并退出"; exit 130' INT
trap 'ABORT_REASON="收到 SIGTERM，停止 holder 并退出"; exit 143' TERM

write_sysfs() {
    local value=$1
    local file=$2

    printf '%s\n' "$value" > "$file" 2>/dev/null
}

read_cgroup_event() {
    local key=$1
    local file="$CGROUP_PATH/memory.events"

    if [[ ! -f "$file" ]]; then
        echo 0
        return
    fi
    awk -v key="$key" '$1 == key {print $2; exit}' "$file"
}

cgroup_swap_current_mb() {
    local value

    if [[ "$CGROUP_TYPE" == "v2" && -f "$CGROUP_PATH/memory.swap.current" ]]; then
        value=$(cat "$CGROUP_PATH/memory.swap.current" 2>/dev/null || echo 0)
        echo $((value / 1024 / 1024))
        return
    fi

    echo ""
}

enable_cgroup() {
    local limit_mb=$1
    local limit_bytes high_bytes

    if [[ "$DISABLE_CGROUP" == "true" ]]; then
        if [[ "$STRICT_SWAP" == "true" ]]; then
            die "STRICT_SWAP=true 时不能禁用 cgroup；否则无法保证 holder 不使用 swap"
        fi
        warn "cgroup 已被显式禁用；当前为 best-effort 模式，无法限制 holder 最大内存，也无法禁止 holder 使用 swap"
        return
    fi

    if [[ $EUID -ne 0 ]]; then
        if [[ "$STRICT_SWAP" == "true" ]]; then
            die "STRICT_SWAP=true 时需要 sudo/root 来配置 cgroup 与禁止 swap"
        fi
        warn "非 root 运行，无法配置 cgroup；当前为 best-effort 模式，无法限制 holder 最大内存，也无法禁止 holder 使用 swap"
        return
    fi

    limit_bytes=$((limit_mb * 1024 * 1024))

    if [[ -f /sys/fs/cgroup/cgroup.controllers ]]; then
        CGROUP_TYPE="v2"
        CGROUP_PATH="/sys/fs/cgroup/$GROUP_NAME"
        mkdir -p "$CGROUP_PATH" || die "无法创建 cgroup: $CGROUP_PATH"
        CGROUP_ATTACH_FILE="$CGROUP_PATH/cgroup.procs"

        if [[ -w /sys/fs/cgroup/cgroup.subtree_control ]]; then
            write_sysfs "+memory" /sys/fs/cgroup/cgroup.subtree_control || true
        fi

        write_sysfs "$limit_bytes" "$CGROUP_PATH/memory.max" || die "无法设置 memory.max"
        if [[ -f "$CGROUP_PATH/memory.high" && "$MEMORY_HIGH_PERCENT" -gt 0 ]]; then
            high_bytes=$((limit_bytes * MEMORY_HIGH_PERCENT / 100))
            write_sysfs "$high_bytes" "$CGROUP_PATH/memory.high" || true
        fi
        if [[ -f "$CGROUP_PATH/memory.swap.max" ]]; then
            write_sysfs 0 "$CGROUP_PATH/memory.swap.max" || die "无法设置 memory.swap.max=0"
        elif [[ "$STRICT_SWAP" == "true" ]]; then
            die "当前 cgroup v2 不支持 memory.swap.max，无法保证不使用 swap"
        else
            warn "当前 cgroup v2 不支持 memory.swap.max；无法禁止 holder 使用 swap"
        fi
        if [[ -f "$CGROUP_PATH/memory.oom.group" ]]; then
            write_sysfs 1 "$CGROUP_PATH/memory.oom.group" || true
        fi

        CGROUP_ENABLED=true
        if [[ -f "$CGROUP_PATH/memory.swap.max" ]]; then
            ok "已配置 cgroup v2：memory.max=${limit_mb}MB，swap.max=0"
        else
            ok "已配置 cgroup v2：memory.max=${limit_mb}MB"
        fi
    elif [[ -d /sys/fs/cgroup/memory ]]; then
        CGROUP_TYPE="v1"
        CGROUP_PATH="/sys/fs/cgroup/memory/$GROUP_NAME"
        mkdir -p "$CGROUP_PATH" || die "无法创建 cgroup: $CGROUP_PATH"
        CGROUP_ATTACH_FILE="$CGROUP_PATH/tasks"

        write_sysfs "$limit_bytes" "$CGROUP_PATH/memory.limit_in_bytes" || die "无法设置 memory.limit_in_bytes"
        if [[ -f "$CGROUP_PATH/memory.swappiness" ]]; then
            write_sysfs 0 "$CGROUP_PATH/memory.swappiness" || true
        fi
        if [[ -f "$CGROUP_PATH/memory.memsw.limit_in_bytes" ]]; then
            write_sysfs "$limit_bytes" "$CGROUP_PATH/memory.memsw.limit_in_bytes" || die "无法设置 memory.memsw.limit_in_bytes"
        elif [[ "$STRICT_SWAP" == "true" ]]; then
            die "当前 cgroup v1 未启用 memsw accounting，无法保证 holder 不使用 swap"
        else
            warn "当前 cgroup v1 未启用 memsw accounting；无法严格禁止 holder 使用 swap"
        fi

        CGROUP_ENABLED=true
        if [[ -f "$CGROUP_PATH/memory.memsw.limit_in_bytes" ]]; then
            ok "已配置 cgroup v1：memory.limit=${limit_mb}MB，memsw.limit=${limit_mb}MB"
        else
            ok "已配置 cgroup v1：memory.limit=${limit_mb}MB"
        fi
    elif [[ "$STRICT_SWAP" == "true" ]]; then
        die "未检测到可用 cgroup，无法保证 holder 不使用 swap"
    else
        warn "未检测到可用 cgroup；当前为 best-effort 模式，无法限制 holder 最大内存，也无法禁止 holder 使用 swap"
    fi
}

detect_nodes() {
    NODE_IDS=()
    NODE_TOTAL_MB=()
    NODE_FREE_MB=()
    NODE_RESERVE_MB=()

    local dir node total free reserve
    for dir in /sys/devices/system/node/node[0-9]*; do
        [[ -d "$dir" ]] || continue
        node=${dir##*node}
        total=$(read_node_meminfo_mb "$node" MemTotal)
        free=$(read_node_meminfo_mb "$node" MemFree)
        [[ -n "$total" && -n "$free" ]] || continue

        reserve=$(node_reserve_mb "$total")

        NODE_IDS+=("$node")
        NODE_TOTAL_MB+=("$total")
        NODE_FREE_MB+=("$free")
        NODE_RESERVE_MB+=("$reserve")
    done

    if [[ ${#NODE_IDS[@]} -eq 0 ]]; then
        local fallback_reserve

        fallback_reserve=$(node_reserve_mb "$TOTAL_MEMORY_MB")
        NODE_IDS=("none")
        NODE_TOTAL_MB=("$TOTAL_MEMORY_MB")
        NODE_FREE_MB=("$AVAILABLE_MEMORY_MB")
        NODE_RESERVE_MB=("$fallback_reserve")
    fi
}

print_nodes() {
    local i

    if [[ ${#NODE_IDS[@]} -le 1 || "${NODE_IDS[0]}" == "none" ]]; then
        log "NUMA: 未检测到多个 node，跳过 NUMA 绑定"
        return
    fi

    log "NUMA node 诊断（仅作观察，不使用 node MemFree 计算占用上限）:"
    for i in "${!NODE_IDS[@]}"; do
        echo "    ├─ node ${NODE_IDS[$i]}: total=${NODE_TOTAL_MB[$i]}MB free=${NODE_FREE_MB[$i]}MB reserve_hint=${NODE_RESERVE_MB[$i]}MB"
    done
}

should_use_numa_interleave() {
    if [[ "$DISABLE_NUMA_SAFETY" == "true" || "$NUMA_POLICY" == "none" ]]; then
        return 1
    fi
    if [[ "$NUMA_POLICY" != "interleave" ]]; then
        warn "未知 NUMA_POLICY=$NUMA_POLICY，将跳过 NUMA 绑定"
        return 1
    fi
    if [[ ${#NODE_IDS[@]} -le 1 || "${NODE_IDS[0]}" == "none" ]]; then
        return 1
    fi
    if ! command -v numactl >/dev/null 2>&1; then
        warn "检测到多个 NUMA node，但未安装 numactl；跳过 NUMA 绑定并按普通全局内存占用执行"
        return 1
    fi
    return 0
}

start_holder() {
    local mb=$1
    local pid

    if [[ $mb -le 0 ]]; then
        return
    fi

    (
        if [[ "$CGROUP_ENABLED" == "true" ]]; then
            echo "$BASHPID" > "$CGROUP_ATTACH_FILE" || exit 97
        fi
        echo 1000 > "/proc/$BASHPID/oom_score_adj" 2>/dev/null || true

        if should_use_numa_interleave; then
            exec nice -n "$NICE_LEVEL" \
                numactl --interleave=all \
                python3 "$HOLDER_FILE" "$mb" "$DURATION" "$HOLDER_CHUNK_MB" "$TOUCH_DELAY_MS" "interleave"
        else
            exec nice -n "$NICE_LEVEL" \
                python3 "$HOLDER_FILE" "$mb" "$DURATION" "$HOLDER_CHUNK_MB" "$TOUCH_DELAY_MS" "global"
        fi
    ) &
    pid=$!
    HOLDER_PIDS+=("$pid")
    if should_use_numa_interleave; then
        log "holder 已启动: pid=$pid policy=interleave size=${mb}MB"
    else
        log "holder 已启动: pid=$pid policy=global size=${mb}MB"
    fi
}

holders_alive() {
    local pid
    for pid in "${HOLDER_PIDS[@]}"; do
        if kill -0 "$pid" 2>/dev/null; then
            return 0
        fi
    done
    return 1
}

monitor_loop() {
    local start_time now elapsed available_mb used_mb percent swap_free pswpout psi
    local base_swap_free=$1
    local _base_pswpin=$2
    local base_pswpout=$3
    local base_oom=0
    local base_oom_kill=0
    local base_max=0
    local cgroup_oom cgroup_oom_kill cgroup_max
    local cgroup_swap_mb swap_pressure_samples=0 psi_pressure_samples=0
    local last_pswpout=$base_pswpout

    if [[ "$CGROUP_TYPE" == "v2" ]]; then
        base_oom=$(read_cgroup_event oom)
        base_oom_kill=$(read_cgroup_event oom_kill)
        base_max=$(read_cgroup_event max)
    fi

    start_time=$(date +%s)
    while true; do
        now=$(date +%s)
        elapsed=$((now - start_time))
        if [[ $elapsed -ge $DURATION ]]; then
            break
        fi

        available_mb=$(available_memory_mb)
        used_mb=$((TOTAL_MEMORY_MB - available_mb))
        percent=$((used_mb * 100 / TOTAL_MEMORY_MB))
        swap_free=$(swap_free_mb)
        pswpout=$(vmstat_value pswpout)
        psi=$(memory_psi_full_avg10)
        cgroup_swap_mb=$(cgroup_swap_current_mb)

        if [[ -n "$cgroup_swap_mb" ]]; then
            echo "[$(date '+%H:%M:%S')] 内存使用: ${used_mb}MB (${percent}%) | 可用: ${available_mb}MB | SwapFree: ${swap_free}MB | cgroup Swap: ${cgroup_swap_mb}MB | PSI full avg10: ${psi}"
        else
            echo "[$(date '+%H:%M:%S')] 内存使用: ${used_mb}MB (${percent}%) | 可用: ${available_mb}MB | SwapFree: ${swap_free}MB | PSI full avg10: ${psi}"
        fi

        if [[ ${#HOLDER_PIDS[@]} -gt 0 ]] && ! holders_alive; then
            ABORT_REASON="所有 holder 进程已提前退出，停止监控"
            return 1
        fi

        if [[ -n "$cgroup_swap_mb" ]]; then
            if [[ $cgroup_swap_mb -gt 0 ]]; then
                swap_pressure_samples=$((swap_pressure_samples + 1))
            else
                swap_pressure_samples=0
            fi
        elif [[ "$GLOBAL_SWAP_ABORT" == "true" ]]; then
            if [[ $((base_swap_free - swap_free)) -gt $SWAP_ABORT_DELTA_MB || $pswpout -gt $last_pswpout ]]; then
                swap_pressure_samples=$((swap_pressure_samples + 1))
            else
                swap_pressure_samples=0
            fi
            last_pswpout=$pswpout
        else
            swap_pressure_samples=0
        fi

        if [[ $swap_pressure_samples -ge $PRESSURE_ABORT_SAMPLES ]]; then
            if [[ -n "$cgroup_swap_mb" ]]; then
                ABORT_REASON="cgroup swap.current=${cgroup_swap_mb}MB 连续异常 ${swap_pressure_samples} 次，主动释放 holder"
            else
                ABORT_REASON="检测到全局 swap 压力连续异常 ${swap_pressure_samples} 次（SwapFree delta=$((base_swap_free - swap_free))MB, pswpout:${base_pswpout}->${pswpout}），主动释放 holder"
            fi
            return 1
        fi

        if [[ -n "$psi" ]] && float_gt "$psi" "$PSI_FULL_AVG10_ABORT"; then
            psi_pressure_samples=$((psi_pressure_samples + 1))
        else
            psi_pressure_samples=0
        fi

        if [[ $psi_pressure_samples -ge $PRESSURE_ABORT_SAMPLES ]]; then
            ABORT_REASON="memory pressure full avg10=${psi} 连续超过阈值 ${PSI_FULL_AVG10_ABORT} 达 ${psi_pressure_samples} 次，主动释放 holder"
            return 1
        fi

        if [[ "$CGROUP_TYPE" == "v2" ]]; then
            cgroup_oom=$(read_cgroup_event oom)
            cgroup_oom_kill=$(read_cgroup_event oom_kill)
            cgroup_max=$(read_cgroup_event max)
            if [[ $cgroup_oom -gt $base_oom || $cgroup_oom_kill -gt $base_oom_kill || $cgroup_max -gt $base_max ]]; then
                ABORT_REASON="cgroup memory.events 出现压力事件 max/oom/oom_kill，主动释放 holder"
                return 1
            fi
        fi

        sleep "$MONITOR_INTERVAL"
    done

    return 0
}

validate_int_range "目标内存占用率" "$TARGET_MEMORY_PERCENT" 1 100
validate_int_range "运行时长" "$DURATION" 1 86400
validate_int_range "nice 值" "$NICE_LEVEL" -20 19
validate_int_range "holder 分块大小" "$HOLDER_CHUNK_MB" 1 1024
validate_int_range "触页暂停毫秒" "$TOUCH_DELAY_MS" 0 10000
validate_int_range "NUMA 保留百分比" "$NODE_RESERVE_PERCENT" 1 90
validate_int_range "NUMA 可用余量占用比例" "$NODE_ALLOC_RATIO_PERCENT" 1 100
validate_int_range "SwapFree 下降退出阈值" "$SWAP_ABORT_DELTA_MB" 0 1048576
validate_int_range "连续压力退出采样数" "$PRESSURE_ABORT_SAMPLES" 1 100
validate_int_range "memory.high 百分比" "$MEMORY_HIGH_PERCENT" 0 100
validate_int_range "cgroup 安全开销 MB" "$SAFETY_OVERHEAD_MB" 0 1048576

if [[ "$MIN_NODE_RESERVE_MB" != "auto" ]]; then
    validate_int_range "NUMA 最小保留 MB" "$MIN_NODE_RESERVE_MB" 0 1048576
fi

if [[ "$(uname -s)" != "Linux" ]]; then
    die "v2 版本当前仅支持 Linux；NUMA、cgroup、swap 防护都依赖 Linux 接口"
fi

command -v python3 >/dev/null 2>&1 || die "缺少 python3，无法启动轻量 memory holder"
[[ -r /proc/meminfo ]] || die "无法读取 /proc/meminfo"

MEM_AVAILABLE_FIELD="MemAvailable"
if [[ -z "$(meminfo_kb "$MEM_AVAILABLE_FIELD")" ]]; then
    warn "当前内核未提供 MemAvailable，将使用 MemFree 进行更保守的可用内存估算"
    MEM_AVAILABLE_FIELD="MemFree"
fi
if [[ -z "$(meminfo_kb "$MEM_AVAILABLE_FIELD")" ]]; then
    die "无法从 /proc/meminfo 获取可用内存字段"
fi

TOTAL_MEMORY_MB=$(( $(meminfo_kb MemTotal) / 1024 ))
AVAILABLE_MEMORY_MB=$(available_memory_mb)
USED_MEMORY_MB=$((TOTAL_MEMORY_MB - AVAILABLE_MEMORY_MB))
CURRENT_MEMORY_PERCENT=$((USED_MEMORY_MB * 100 / TOTAL_MEMORY_MB))

echo "=========================================="
echo "[*] 内存占用保持工具 v2 - 无外部压测工具"
echo "=========================================="
log "总内存: ${TOTAL_MEMORY_MB}MB"
log "可用内存: ${AVAILABLE_MEMORY_MB}MB"
log "当前占用率: ${CURRENT_MEMORY_PERCENT}%"
log "目标占用率: ${TARGET_MEMORY_PERCENT}%"
log "运行时长: ${DURATION}s"
log "nice 优先级: ${NICE_LEVEL}"

if [[ "$MEMORY_SIZE" == "auto" ]]; then
    TARGET_MEMORY_MB=$((TOTAL_MEMORY_MB * TARGET_MEMORY_PERCENT / 100))
    NEEDED_MEMORY_MB=$((TARGET_MEMORY_MB - USED_MEMORY_MB))
else
    NEEDED_MEMORY_MB=$(parse_size_mb "$MEMORY_SIZE") || die "无效的内存大小格式: $MEMORY_SIZE"
fi

detect_nodes

if [[ $NEEDED_MEMORY_MB -le 0 ]]; then
    ok "当前内存占用率已达到目标，无需额外占用；仅监控 ${DURATION}s"
    monitor_loop "$(swap_free_mb)" "$(vmstat_value pswpin)" "$(vmstat_value pswpout)"
    exit $?
fi

if [[ $NEEDED_MEMORY_MB -lt 10 ]]; then
    NEEDED_MEMORY_MB=10
fi

print_nodes

MAX_SAFE_MEMORY=$((AVAILABLE_MEMORY_MB * NODE_ALLOC_RATIO_PERCENT / 100))
if [[ $NEEDED_MEMORY_MB -gt $MAX_SAFE_MEMORY ]]; then
    if [[ "$MEMORY_SIZE" == "auto" ]]; then
        warn "按全局可用内存安全比例限制申请内存为 ${MAX_SAFE_MEMORY}MB，低于目标需要的 ${NEEDED_MEMORY_MB}MB"
        NEEDED_MEMORY_MB=$MAX_SAFE_MEMORY
    else
        warn "用户指定 ${NEEDED_MEMORY_MB}MB 超过当前全局安全建议 ${MAX_SAFE_MEMORY}MB；仍按用户指定执行，并依赖监控止损"
    fi
fi

if should_use_numa_interleave; then
    log "NUMA 策略: numactl --interleave=all"
else
    log "NUMA 策略: global（不使用 numactl 绑定）"
fi
log "holder 分配计划: ${NEEDED_MEMORY_MB}MB"

CGROUP_LIMIT_MB=$((NEEDED_MEMORY_MB + SAFETY_OVERHEAD_MB + HOLDER_CHUNK_MB))
enable_cgroup "$CGROUP_LIMIT_MB"
create_holder_file

BASE_SWAP_FREE=$(swap_free_mb)
BASE_PSWPIN=$(vmstat_value pswpin)
BASE_PSWPOUT=$(vmstat_value pswpout)

start_holder "$NEEDED_MEMORY_MB"

if monitor_loop "$BASE_SWAP_FREE" "$BASE_PSWPIN" "$BASE_PSWPOUT"; then
    ok "已完成 ${DURATION}s 内存占用保持"
else
    exit 2
fi
