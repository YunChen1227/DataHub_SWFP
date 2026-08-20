#!/usr/bin/env bash
# DataHub relay 重启脚本（Linux ECS）。
#
# 流程（构建全部前置：任一构建失败则旧服务继续运行，不产生服务空窗）：
#   1. go mod download + go build 重新编译后端（失败即退出，不动旧进程）
#   2. npm install + npm run build 构建管理前端 web/admin -> dist（SKIP_WEB=1 可跳过）
#   3. 停止旧服务（TERM → 最多等 10s → KILL），三层兜底：pid 文件 → 按二进制路径
#      pgrep → 按「谁在监听 $PORT」；最后确认端口已释放才继续。历史教训：旧进程若
#      不是本脚本启动的（如手动 nohup ./relay，二进制路径不同），前两层都找不到它，
#      新进程 bind: address already in use 秒退，而旧进程还在替它响应健康检查，
#      形成"重启成功"的假象。
#   4. nohup 后台启动服务，日志追加到 logs/relay.log
#   5. 健康检查：/healthz 与管理界面 /admin/ 均返回 200，且监听 $PORT 的确实是
#      本次启动的新进程，才算成功
#
# 用法：
#   ./scripts/restart.sh                          # 默认 CONFIG_FILE=config.aliyun.prod.yaml
#   SKIP_WEB=1 ./scripts/restart.sh               # 前端没改动时跳过 npm 构建（更快）
#   CONFIG_FILE=config.yaml ./scripts/restart.sh  # 指定其它配置
#   ADDR=:8081 ./scripts/restart.sh               # 覆盖端口（缺省读配置文件的 addr）
#
# 首次使用：chmod +x scripts/restart.sh（需已安装 go 与 node/npm）

set -euo pipefail

# --- 定位仓库根目录（脚本在 scripts/ 下） ---
REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_DIR"

CONFIG_FILE="${CONFIG_FILE:-config.aliyun.prod.yaml}"
BIN="$REPO_DIR/bin/relay"
PID_FILE="$REPO_DIR/relay.pid"
LOG_DIR="$REPO_DIR/logs"
LOG_FILE="$LOG_DIR/relay.log"

# 国内 ECS 直连 proxy.golang.org 常超时，未显式设置时用 goproxy.cn
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"

log() { echo "[$(date '+%F %T')] $*"; }
fail() { log "错误: $*" >&2; exit 1; }

[ -f "$CONFIG_FILE" ] || fail "配置文件 $CONFIG_FILE 不存在（工作目录 $REPO_DIR）"

# 监听端口以配置文件的顶层 addr 为准（ADDR 环境变量可覆盖）。绝不能硬编码默认值：
# PORT 同时用于「按端口清理残留进程」，同机部署多个 relay 时若取错端口，会把另一个
# 服务当成残留进程杀掉（历史事故：SWFP 用默认 8080 扫，杀掉了跑在 8080 的主服务）。
if [ -n "${ADDR:-}" ]; then
    ADDR_SRC="ADDR 环境变量"
else
    ADDR="$(sed -nE 's/^addr:[[:space:]]*"?([^"#[:space:]]+)"?.*/\1/p' "$CONFIG_FILE" | head -n1)"
    ADDR_SRC="$CONFIG_FILE 的 addr"
    [ -n "$ADDR" ] || fail "无法从 $CONFIG_FILE 解析顶层 addr，请显式指定 ADDR=:端口"
fi
PORT="${ADDR##*:}"
case "$PORT" in
    ''|*[!0-9]*) fail "端口非法: ADDR=$ADDR (取自 $ADDR_SRC)" ;;
esac
log "监听端口 $PORT (addr=$ADDR, 取自 $ADDR_SRC)"

# --- 1. 编译后端（先于杀进程：编译失败不影响正在运行的旧服务） ---
log "== 1/5 编译后端 =="
log "go mod download ..."
go mod download || fail "go mod download 失败"
mkdir -p "$(dirname "$BIN")"
log "go build -> $BIN ..."
go build -o "$BIN" ./cmd/relay || fail "go build 失败，旧服务未受影响"
log "后端编译完成"

# --- 2. 构建管理前端（同样先于杀进程；SKIP_WEB=1 跳过） ---
log "== 2/5 构建管理前端 =="
WEB_DIR="$REPO_DIR/web/admin"
if [ "${SKIP_WEB:-0}" = "1" ]; then
    log "SKIP_WEB=1，跳过前端构建（沿用现有 web/admin/dist）"
elif [ ! -d "$WEB_DIR" ]; then
    log "警告: $WEB_DIR 不存在，跳过前端构建"
else
    command -v npm >/dev/null 2>&1 || fail "未安装 npm（管理前端构建需要 node/npm；临时可用 SKIP_WEB=1 跳过）"
    # 国内 ECS 直连 npmjs 常超时；未显式配置镜像时用 npmmirror（仅本次 install 生效）
    NPM_REG="${NPM_REGISTRY:-https://registry.npmmirror.com}"
    log "npm install (registry=$NPM_REG) ..."
    (cd "$WEB_DIR" && npm install --no-audit --no-fund --registry="$NPM_REG") \
        || fail "npm install 失败，旧服务未受影响"
    log "npm run build ..."
    (cd "$WEB_DIR" && npm run build) || fail "npm run build 失败，旧服务未受影响"
    [ -d "$WEB_DIR/dist" ] || fail "构建完成但 $WEB_DIR/dist 不存在，检查 vite 输出目录"
    log "前端构建完成 -> web/admin/dist"
fi

# --- 3. 停止旧进程 ---
log "== 3/5 停止旧服务 =="
stop_pid() {
    local pid="$1"
    if ! kill -0 "$pid" 2>/dev/null; then
        return 0
    fi
    log "发送 TERM 到 pid=$pid ..."
    kill "$pid" 2>/dev/null || true
    for _ in $(seq 1 20); do
        kill -0 "$pid" 2>/dev/null || { log "pid=$pid 已退出"; return 0; }
        sleep 0.5
    done
    log "10s 未退出，强制 KILL pid=$pid"
    kill -9 "$pid" 2>/dev/null || true
}

# listen_pids 列出当前监听 $PORT 的进程 pid（ss 常驻 Ubuntu；lsof/fuser 兜底）。
listen_pids() {
    if command -v ss >/dev/null 2>&1; then
        ss -ltnp 2>/dev/null | grep -E "[:*]$PORT[[:space:]]" | grep -oE 'pid=[0-9]+' | cut -d= -f2 | sort -u
    elif command -v lsof >/dev/null 2>&1; then
        lsof -t -iTCP:"$PORT" -sTCP:LISTEN 2>/dev/null | sort -u
    elif command -v fuser >/dev/null 2>&1; then
        fuser "$PORT"/tcp 2>/dev/null | tr -s ' \t' '\n' | grep -E '^[0-9]+$' | sort -u
    fi
}

if [ -f "$PID_FILE" ]; then
    OLD_PID="$(cat "$PID_FILE" 2>/dev/null || true)"
    [ -n "$OLD_PID" ] && stop_pid "$OLD_PID"
    rm -f "$PID_FILE"
fi
# 兜底 1：pid 文件丢失/被删时按二进制路径匹配残留进程
for pid in $(pgrep -f "$BIN" 2>/dev/null || true); do
    stop_pid "$pid"
done
# 兜底 2：按端口清理。旧进程若不是本脚本启动的（无 pid 文件、二进制路径不同，
# 例如手动 go build -o relay 后 nohup ./relay），上面两层都找不到它，只有按
# 「谁在监听 $PORT」才能定位；不清掉它，新进程会 bind 失败秒退。
for pid in $(listen_pids); do
    log "端口 $PORT 仍被 pid=$pid 占用（$(ps -p "$pid" -o args= 2>/dev/null || echo 未知进程)），停止之"
    stop_pid "$pid"
done

# 确认端口确实释放（TIME_WAIT 不影响 bind，只看 LISTEN），未释放则放弃启动
for _ in $(seq 1 20); do
    [ -z "$(listen_pids)" ] && break
    sleep 0.5
done
if [ -n "$(listen_pids)" ]; then
    ss -ltnp 2>/dev/null | grep -E "[:*]$PORT[[:space:]]" >&2 || true
    fail "端口 $PORT 仍被上述进程占用，放弃启动（请手动排查后重跑）"
fi
log "旧服务已全部停止，端口 $PORT 已释放"

# --- 4. 后台启动 ---
log "== 4/5 后台启动 =="
mkdir -p "$LOG_DIR"
[ -d "web/admin/dist" ] || log "警告: web/admin/dist 不存在，管理界面将不可用"
nohup env CONFIG_FILE="$CONFIG_FILE" "$BIN" >> "$LOG_FILE" 2>&1 &
NEW_PID=$!
echo "$NEW_PID" > "$PID_FILE"
log "已启动 pid=$NEW_PID，日志: $LOG_FILE"

# --- 5. 健康检查 ---
log "== 5/5 健康检查 =="
BASE="http://127.0.0.1:${PORT}"

wait_http_200() {
    local url="$1" name="$2" tries="${3:-30}"
    for _ in $(seq 1 "$tries"); do
        # 先确认新进程还活着，再 curl：否则残留旧进程替它返回 200，
        # 会把"新进程已崩溃"误判为重启成功（历史事故）。
        if ! kill -0 "$NEW_PID" 2>/dev/null; then
            log "进程 pid=$NEW_PID 已退出，最近日志："
            tail -n 20 "$LOG_FILE" >&2 || true
            fail "$name 健康检查失败（进程崩溃）"
        fi
        code="$(curl -s -o /dev/null -w '%{http_code}' -m 3 "$url" 2>/dev/null || true)"
        if [ "$code" = "200" ]; then
            log "OK  $name ($url -> 200)"
            return 0
        fi
        sleep 1
    done
    log "最近日志："
    tail -n 20 "$LOG_FILE" >&2 || true
    fail "$name 健康检查超时（$url 未返回 200）"
}

wait_http_200 "$BASE/healthz" "主服务 /healthz"
wait_http_200 "$BASE/admin/"  "管理界面 /admin/"

# 终验：监听 $PORT 的必须是本次启动的新进程（防任何形态的旧进程"替身"）。
LISTENER="$(listen_pids | head -n1 || true)"
if [ -n "$LISTENER" ] && [ "$LISTENER" != "$NEW_PID" ]; then
    fail "端口 $PORT 由 pid=$LISTENER 监听而非新进程 pid=$NEW_PID，重启未生效（请排查残留进程）"
fi

log "全部通过：服务已在后台运行 (pid=$NEW_PID, addr=$ADDR, config=$CONFIG_FILE)"
