#!/usr/bin/env bash
# 真实环境联调入口：不启 mock，relay 直连上游 + 跑 13_ctax_real.go。
#
# 用法：
#   ./test/run_real.sh
#   CONFIG_FILE=config.local.real.yaml ./test/run_real.sh
#
# 生产源6（需在 ECS 白名单内）：
#   CTAX_BASE_URL=https://api.huizhongcredit.com/hzservice/sy/tax \
#   CTAX_APP_ID=9VTYC6YU CTAX_TOKEN='...' CTAX_AUTH_CODE=gckj \
#   ./test/run_real.sh
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO"

CONFIG_FILE="${CONFIG_FILE:-config.local.real.yaml}"
RESULT_DIR="${RESULT_DIR:-test_res/$(date +%F)-real}"
mkdir -p "$RESULT_DIR"

export CONFIG_FILE
export RESULT_DIR
export RELAY_BASE_URL="${RELAY_BASE_URL:-http://localhost:8080}"

echo "== DataHub_SWFP REAL upstream test =="
echo "  config    : $CONFIG_FILE"
echo "  resultDir : $RESULT_DIR"
echo "  (no mocks — relay talks to real upstreams)"

cleanup() {
  [[ -n "${RELAY_PID:-}" ]] && kill "$RELAY_PID" 2>/dev/null || true
}
trap cleanup EXIT

go build -o "$RESULT_DIR/relay" ./cmd/relay
CONFIG_FILE="$CONFIG_FILE" "$RESULT_DIR/relay" >"$RESULT_DIR/relay.log" 2>&1 &
RELAY_PID=$!

for i in $(seq 1 40); do
  if curl -sf "$RELAY_BASE_URL/healthz" >/dev/null 2>&1; then
    echo "relay is up."
    break
  fi
  if [[ $i -eq 40 ]]; then
    echo "relay /healthz not ready; see $RESULT_DIR/relay.log"
    exit 1
  fi
  sleep 0.5
done

echo "---- probe_ctax (xlsx batch) ----"
go run ./scripts/probe_ctax.go 2>&1 | tee "$RESULT_DIR/probe_ctax.log" || {
  ec=$?
  if [[ $ec -eq 2 ]]; then
    echo "probe_ctax: 鉴权/授权配置错误 (exit $ec)"
    exit $ec
  fi
  if [[ $ec -eq 3 ]]; then
    echo "probe_ctax: 上游非预期失败 (exit $ec)"
    exit $ec
  fi
}

echo "---- 13_ctax_real ----"
RELAY_BASE_URL="$RELAY_BASE_URL" go run test/cases/13_ctax_real.go 2>&1 | tee "$RESULT_DIR/13_ctax_real.log"

echo "== done. logs in $RESULT_DIR =="
