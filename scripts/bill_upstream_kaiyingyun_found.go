//go:build ignore

// bill_upstream_kaiyingyun_found: 凯盈云（provider=salesdata）查得口径月度对账单（只读库，不重调、不判金额）。
//
//	CONFIG_FILE=config.aliyun.prod.yaml MONTHS=2026-09,2026-10 go run ./scripts/bill_upstream_kaiyingyun_found.go
//
// 直接查库：upstream_call 当月 provider=salesdata 且 status=ok 的调用（不分下游客户）。凯盈云只出发票，
// 一律计入 只调用票。查得即计费，出表：OUT_DIR/凯盈云-月度对账单-查得口径-<月>.xlsx。
//
// 可选环境变量：ISSUER（缺省 凯盈云）、OUR_NAME（客户名称=我方）、OUT_DIR、DEDUPE。
package main

import "github.com/datahub/relay/internal/tools/bill24"

func main() { bill24.Main(func() error { return bill24.RunUpstreamFound("salesdata") }) }
