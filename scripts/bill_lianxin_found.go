//go:build ignore

// bill_lianxin_found: 北京联信 查得口径月度对账单（只读库，不重调、不判金额）。
//
//	CONFIG_FILE=config.aliyun.prod.yaml MONTHS=2026-09,2026-10 go run ./scripts/bill_lianxin_found.go
//
// 步骤：
//  1. 直接查库：北京联信（license.name 模糊匹配）当月 found_data 的请求，按 req_scope 分
//     只调用票 / 只调用税 / 同时调用票税，记录税号与请求时间。
//  2. 查得即计费，按冠辰月度对账单格式出表，每月一个文件：
//     OUT_DIR/北京联信-月度对账单-查得口径-<月>.xlsx。
//
// 可选环境变量：CUSTOMER（缺省 北京联信）、ISSUER（标题出单方）、OUT_DIR（缺省 test_res/bill_found）、
// DEDUPE（缺省 1：同月同 (税号, 接口) 只计首次；0：每次调用都列出）。
package main

import "github.com/datahub/relay/internal/tools/bill24"

func main() { bill24.Main(bill24.RunDownstreamFound) }
