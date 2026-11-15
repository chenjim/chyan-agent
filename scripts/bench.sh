#!/usr/bin/env bash
# A12 性能实测：CLI 冷启动、daemon 空闲 RSS、六目标交叉编译与产物校验。
# 用法：scripts/bench.sh [runs]   （runs 默认 10，冷启动采样次数）
# 输出人读结果，同时写出 JSON 摘要到 dist/bench.json（供归档/对比）。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SRC="$ROOT/chyan"
OUT="$ROOT/dist"
RUNS="${1:-10}"

mkdir -p "$OUT"
TMP="$(mktemp -d)"
PID=""
cleanup() {
  [[ -n "$PID" ]] && kill "$PID" 2>/dev/null || true
  rm -rf "$TMP"
}
trap cleanup EXIT

echo "== 环境 =="
printf 'host     %s\n' "$(uname -srm)"
printf 'cpu      %s\n' "$(grep -m1 'model name' /proc/cpuinfo | cut -d: -f2 | sed 's/^ //')"
printf 'cores    %s\n' "$(nproc)"
printf 'go       %s\n' "$(go version | awk '{print $3}')"
printf 'commit   %s\n' "$(git -C "$ROOT" rev-parse --short HEAD)"

echo
echo "== 构建（与 release 同口径：CGO_ENABLED=0 -trimpath）=="
BIN="$TMP/chyan"
(cd "$SRC" && CGO_ENABLED=0 go build -trimpath -o "$BIN" ./cmd/chyan)
printf 'binary   %s (%s)\n' "$BIN" "$(du -h "$BIN" | cut -f1)"

# --- 1. CLI 冷启动：每次独立进程，取中位数 -------------------------------
echo
echo "== CLI 冷启动：chyan version × $RUNS =="
mkdir -p "$TMP/home"
times=()
for _ in $(seq 1 "$RUNS"); do
  start=$(date +%s%N)
  CHYAN_HOME="$TMP/home" CHYAN_LOG_LEVEL=off "$BIN" version >/dev/null
  end=$(date +%s%N)
  times+=($(( (end - start) / 1000000 )))
done
sorted=($(printf '%s\n' "${times[@]}" | sort -n))
median="${sorted[$(( ${#sorted[@]} / 2 ))]}"
min="${sorted[0]}"
max="${sorted[${#sorted[@]}-1]}"
echo "samples(ms): ${times[*]}"
echo "min=${min}ms median=${median}ms max=${max}ms (阈值 300ms)"
if (( median >= 300 )); then
  echo "FAIL: CLI 冷启动超出 300ms" >&2
else
  echo "PASS: CLI 冷启动 ${median}ms < 300ms"
fi

# --- 2. daemon 空闲 RSS ---------------------------------------------------
echo
echo "== daemon 空闲 RSS =="
DPORT=31490
cat >"$TMP/home/config.toml" <<EOF
[llm]
base_url = "http://127.0.0.1:1/v1"
model = "bench"
api_key = ""
EOF
CHYAN_HOME="$TMP/home" CHYAN_LOG_LEVEL=off "$BIN" serve --token bench --tcp-addr "127.0.0.1:$DPORT" >"$TMP/serve.log" 2>&1 &
PID=$!
for _ in $(seq 1 50); do
  curl -sf -o /dev/null -H 'Authorization: Bearer bench' "http://127.0.0.1:$DPORT/api/v1/status" && break
  sleep 0.2
done
sleep 3 # 稳态：跳过启动期的一次性分配
rss_kb=$(ps -o rss= -p "$PID" | tr -d ' ')
rss_mb=$(awk -v kb="$rss_kb" 'BEGIN { printf "%.1f", kb / 1024 }')
echo "pid=$PID rss=${rss_mb}MB (${rss_kb}KB) (阈值 80MB)"
if awk -v mb="$rss_mb" 'BEGIN { exit !(mb < 80) }'; then
  echo "PASS: daemon 空闲 RSS ${rss_mb}MB < 80MB"
else
  echo "FAIL: daemon 空闲 RSS 超出 80MB" >&2
fi
# 空闲时不应有对外连接（只允许本机监听/回环）。
echo "connections:"
ss -tnp 2>/dev/null | awk -v pid="$PID" 'NR>1 && $0 ~ ("pid=" pid "[,)]")' || true
kill "$PID" 2>/dev/null || true
wait "$PID" 2>/dev/null || true
PID=""

# --- 3. 交叉编译矩阵 -----------------------------------------------------
echo
echo "== 交叉编译矩阵（六目标）=="
targets=(linux/amd64 linux/arm64 windows/amd64 windows/arm64 darwin/amd64 darwin/arm64)
for t in "${targets[@]}"; do
  os="${t%/*}"
  arch="${t#*/}"
  name="chyan_bench_${os}_${arch}"
  [[ "$os" == windows ]] && name="${name}.exe"
  (cd "$SRC" && CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -o "$OUT/$name" ./cmd/chyan)
  printf '%-22s %-8s %s\n' "$name" "$(du -h "$OUT/$name" | cut -f1)" "$(file -b "$OUT/$name" | cut -c1-60)"
done
# 本机目标必须真跑一次；其余目标只校验产物格式（跨平台运行需真机/虚拟机）。
echo "host artifact smoke:"
CHYAN_HOME="$TMP/home" CHYAN_LOG_LEVEL=off "$OUT/chyan_bench_linux_amd64" version

rm -f "$OUT"/chyan_bench_*

# --- 4. 遥测扫描（A12「无遥测外发」）-----------------------------------
echo
echo "== 遥测/上报代码扫描 =="
hits=$(grep -rInE 'sentry|posthog|segment\.io|amplitude|google-analytics|otlp|opentelemetry' \
  "$SRC/internal" "$SRC/cmd" 2>/dev/null | grep -v '_test.go' || true)
if [[ -z "$hits" ]]; then
  echo "PASS: 未发现遥测/上报 SDK 或 OTLP 导出代码"
else
  echo "REVIEW: 命中以下引用（需人工确认非上报路径）："
  echo "$hits"
fi

# --- 摘要 ---------------------------------------------------------------
cat >"$OUT/bench.json" <<EOF
{
  "commit": "$(git -C "$ROOT" rev-parse --short HEAD)",
  "cli_cold_start_ms": {"min": $min, "median": $median, "max": $max, "runs": $RUNS},
  "daemon_idle_rss_mb": $rss_mb,
  "thresholds": {"cli_ms": 300, "daemon_rss_mb": 80}
}
EOF
echo
echo "summary: $OUT/bench.json"
