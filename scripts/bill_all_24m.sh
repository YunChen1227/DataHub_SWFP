#!/usr/bin/env bash
# bill_all_24m.sh: 一次性跑完全部 24 个月口径月度对账单脚本（北京联信 + 4 个上游）。
# 在仓库根目录、已进各上游 IP 白名单的阿里云 ECS 上运行。
#
#   CONFIG_FILE=config.aliyun.prod.yaml MONTHS=2026-09,2026-10 bash scripts/bill_all_24m.sh
#
# 环境变量（全部可选，透传给各脚本）：
#   CONFIG_FILE  缺省 config.aliyun.prod.yaml
#   MONTHS       缺省 2026-09,2026-10
#   OUT_DIR      缺省 test_res/bill24
#   WORKERS      并发，缺省 6
#   LIMIT        每月只复核前 N 条（试跑用），缺省不限
#   ONLY         只跑逗号分隔的子集，如 ONLY=lianxin,huizhong；缺省全跑
#
# 每个脚本独立运行、互不影响：某个失败不会中断其余，最后统一汇总退出码。
# 各脚本自身可断点续跑，重跑本脚本只会补未完成和失败的记录。
set -u

export CONFIG_FILE="${CONFIG_FILE:-config.aliyun.prod.yaml}"
export MONTHS="${MONTHS:-2026-09,2026-10}"
export OUT_DIR="${OUT_DIR:-test_res/bill24}"
export WORKERS="${WORKERS:-6}"

cd "$(dirname "$0")/.." || exit 1

# 名称 -> 脚本文件
names=(lianxin zhengtong huizhong shuiyou kaiyingyun)
declare -A script=(
  [lianxin]=scripts/bill_lianxin_24m.go
  [zhengtong]=scripts/bill_upstream_zhengtong_24m.go
  [huizhong]=scripts/bill_upstream_huizhong_24m.go
  [shuiyou]=scripts/bill_upstream_shuiyou_24m.go
  [kaiyingyun]=scripts/bill_upstream_kaiyingyun_24m.go
)

# ONLY 过滤
run_names=("${names[@]}")
if [ -n "${ONLY:-}" ]; then
  IFS=',' read -ra run_names <<< "$ONLY"
fi

echo "== 24 月口径月度对账单：批量运行 =="
echo "   CONFIG_FILE=$CONFIG_FILE  MONTHS=$MONTHS  OUT_DIR=$OUT_DIR  WORKERS=$WORKERS  LIMIT=${LIMIT:-（不限）}"
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
[ "$overall" -eq 0 ] && echo "   全部成功" || echo "   有脚本失败，见上方日志；修复后重跑本脚本会自动续跑"
exit "$overall"
