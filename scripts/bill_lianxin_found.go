//go:build ignore

// bill_lianxin_found: 北京联信 查得口径对账单（只读库，不重调、不判金额）。
// 默认把 2026-09-01 至 2026-11-16（含当天）的全部查得明细收进一个文件，不按月拆。
//
//	CONFIG_FILE=config.aliyun.prod.yaml go run ./scripts/bill_lianxin_found.go
//
// 步骤：
//  1. 直接查库：北京联信（license.name 模糊匹配）在区间内 found_data 的请求，按 req_scope 分
//     只调用票 / 只调用税 / 同时调用票税，每次查得都保留税号与请求时间。
//  2. 查得即计费，按冠辰月度对账单格式出一张表：
//     OUT_DIR/北京联信-月度对账单-查得口径-2026-09-01_2026-11-16.xlsx。
//
// 可选环境变量：CUSTOMER（缺省 北京联信）、FROM / TO（缺省 2026-09-01 / 2026-11-16，含首尾）、
// ISSUER（标题出单方）、OUT_DIR（缺省 test_res/bill_found）、
// DEDUPE（缺省 0：每次调用都列出；1：整个区间内同 (税号, 接口) 只计首次）。
package main

import (
	"os"

	"github.com/datahub/relay/internal/tools/bill24"
)

func main() {
	if os.Getenv("FROM") == "" {
		os.Setenv("FROM", "2026-09-01")
	}
	if os.Getenv("TO") == "" {
		os.Setenv("TO", "2026-11-16")
	}
	if os.Getenv("DEDUPE") == "" {
		os.Setenv("DEDUPE", "0")
	}
	bill24.Main(bill24.RunDownstreamFound)
}
