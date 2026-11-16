#!/usr/bin/env bash
# bill_all_found.sh: 一次性跑完全部查得口径月度对账单脚本（北京联信 + 4 个上游）。
# 只读库，不重调、不判金额；在能连生产库的阿里云 ECS 上、仓库根目录运行。
#
#   CONFIG_FILE=config.aliyun.prod.yaml MONTHS=2026-09,2026-10 bash scripts/bill_all_found.sh
#
# 环境变量（全部可选，透传给各脚本）：
#   CONFIG_FILE  缺省 config.aliyun.prod.yaml
#   MONTHS       缺省 2026-09,2026-10
#   OUT_DIR      缺省 test_res/bill_found
#   DEDUPE       缺省 1（同月同税号同接口只计首次）；0 为每次调用都列出
#   ONLY         只跑逗号分隔的子集，如 ONLY=lianxin,huizhong；缺省全跑
#
# 某个脚本失败不会中断其余，最后统一汇总退出码。
set -u

export CONFIG_FILE="${CONFIG_FILE:-config.aliyun.prod.yaml}"
export MONTHS="${MONTHS:-2026-09,2026-10}"
export OUT_DIR="${OUT_DIR:-test_res/bill_found}"
export DEDUPE="${DEDUPE:-1}"

cd "$(dirname "$0")/.." || exit 1

names=(lianxin zhengtong huizhong shuiyou kaiyingyun)
declare -A script=(
  [lianxin]=scripts/bill_lianxin_found.go
  [zhengtong]=scripts/bill_upstream_zhengtong_found.go
  [huizhong]=scripts/bill_upstream_huizhong_found.go
  [shuiyou]=scripts/bill_upstream_shuiyou_found.go
  [kaiyingyun]=scripts/bill_upstream_kaiyingyun_found.go
)

run_names=("${names[@]}")
if [ -n "${ONLY:-}" ]; then
  IFS=',' read -ra run_names <<< "$ONLY"
fi

echo "== 查得口径月度对账单：批量运行 =="
echo "   CONFIG_FILE=$CONFIG_FILE  MONTHS=$MONTHS  OUT_DIR=$OUT_DIR  DEDUPE=$DEDUPE"
echo "   将运行：${run_names[*]}"
echo

declare -A status
overall=0
for name in "${run_names[@]}"; do
  name="$(echo "$name" | tr -d '[:space:]')"
  f="${script[$name]:-}"
  if [ -z "$f" ]; then
    echo "!! 跳过未知脚本名：$name" >&2
    status[$name]="未知"
    overall=1
    continue
  fi
  echo "------------------------------------------------------------"
  echo ">> [$name] go run ./$f"
  echo "------------------------------------------------------------"
  if go run "./$f"; then
    status[$name]="成功"
  else
    status[$name]="失败(exit=$?)"
    overall=1
  fi
  echo
done

echo "============================================================"
echo "== 汇总 =="
for name in "${run_names[@]}"; do
  name="$(echo "$name" | tr -d '[:space:]')"
  printf "   %-12s %s\n" "$name" "${status[$name]:-未运行}"
done
echo "   产物目录：$OUT_DIR（每个对象每月一个 xlsx）"
[ "$overall" -eq 0 ] && echo "   全部成功" || echo "   有脚本失败，见上方日志"
exit "$overall"
