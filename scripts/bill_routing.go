//go:build ignore

// bill_routing: 列出区间内每个税号分别调用/命中了哪些上游（只读库）。
// 默认统计北京联信在 2026-09-01 至 2026-11-16 的调用。
//
//	CONFIG_FILE=config.aliyun.prod.yaml go run ./scripts/bill_routing.go
//
// 输出一个文件 OUT_DIR/<客户>-税号上游路由-<区间>.xlsx，三页：
//   - 税号-上游矩阵：每行一个税号，证通/惠众/税友/凯盈云各一列；
//     单元格 ✓票税 表示该上游命中（含查得维度），✗<状态> 表示调用了但未查得；末列是命中上游与调用上游数。
//   - 调用明细：每次上游调用一行（税号、上游、状态、票/税、时间、下游 appKey、requestId）。
//   - 汇总：税号总数、各上游命中税号数。
//
// 可选环境变量：CUSTOMER（下游客户，缺省 北京联信；设为 ALL 则不分客户、全量统计）、
// FROM / TO（yyyy-MM-dd，含首尾，缺省 2026-09-01 / 2026-11-16）、OUT_DIR（缺省 test_res/bill_found）。
package main

import "github.com/datahub/relay/internal/tools/bill24"

func main() { bill24.Main(bill24.RunRouting) }
