//go:build ignore

// bill_upstream_kaiyingyun_24m: 凯盈云（销项数据，provider=salesdata）24 个月口径月度对账单。
// 必须在出口 IP 已进凯盈云白名单的阿里云 ECS 上运行。
//
//	CONFIG_FILE=config.aliyun.prod.yaml MONTHS=2026-09,2026-10 go run ./scripts/bill_upstream_kaiyingyun_24m.go
//
// 步骤：
//  1. 直接查库：upstream_call 当月 provider=salesdata 且 status=ok 的调用（不分下游客户）。
//     凯盈云只提供发票，接口均为「只调用票」，同一税号只取首次。
//  2. 用配置里的凯盈云凭证直连重调；以请求月为锚取 [请求月-24, 请求月] 窗口计算开票金额。
//  3. 金额 <= 0 不需向凯盈云缴费；查无/失败列入「待确认」。
//  4. 按冠辰月度对账单格式出表：OUT_DIR/凯盈云-月度对账单-<月>.xlsx。
//
// 可选环境变量：ISSUER（标题出单方，缺省 凯盈云）、OUR_NAME（客户名称=我方）、OUT_DIR、WORKERS、LIMIT。
package main

import "github.com/datahub/relay/internal/tools/bill24"

func main() { bill24.Main(func() error { return bill24.RunUpstream("salesdata") }) }
