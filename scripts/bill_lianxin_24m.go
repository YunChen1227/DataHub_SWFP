//go:build ignore

// bill_lianxin_24m: 北京联信 24 个月口径月度对账单（只读库 + 经 swfp 中继重调）。
//
//	CONFIG_FILE=config.aliyun.prod.yaml MONTHS=2026-09,2026-10 go run ./scripts/bill_lianxin_24m.go
//
// 步骤：
//  1. 直接查库：北京联信（license.name 模糊匹配）当月 found_data 的请求，按 req_scope 分
//     只调用票 / 只调用税 / 同时调用票税，同一 (税号, 接口) 只取首次请求时间。
//  2. 用 SWFP_APP_KEY / SWFP_SECRET 经 RELAY_BASE_URL 重调 querySrmxSWFP（dataType 同当时的接口），
//     以请求月为锚取 [请求月-24, 请求月] 窗口，计算该接口范围内的总金额。
//  3. 总金额 <= 0 不计费；查无/未取得/失败列入「待确认」。
//  4. 按冠辰月度对账单格式出表，每月一个文件：OUT_DIR/北京联信-月度对账单-<月>.xlsx。
//
// 可选环境变量：CUSTOMER（缺省 北京联信）、ISSUER（标题出单方）、OUT_DIR（缺省 test_res/bill24）、
// WORKERS（缺省 6）、LIMIT（每月只复核前 N 条，试跑用）。断点续跑：已完成的结果记在
// OUT_DIR/<客户>/<月>/progress.jsonl，重跑只补未完成和失败的。
package main

import "github.com/datahub/relay/internal/tools/bill24"

func main() { bill24.Main(bill24.RunDownstream) }
