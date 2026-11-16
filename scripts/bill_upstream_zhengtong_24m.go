//go:build ignore

// bill_upstream_zhengtong_24m: 证通（entcredit，发票 P0130083 / 税务 P0130084）24 个月口径月度对账单。
// 必须在出口 IP 已进证通白名单的阿里云 ECS 上运行。
//
//	CONFIG_FILE=config.aliyun.prod.yaml MONTHS=2026-09,2026-10 go run ./scripts/bill_upstream_zhengtong_24m.go
//
// 步骤：
//  1. 直接查库：upstream_call 当月 provider=entcredit 且 status=ok 的调用（不分下游客户）；
//     同一请求里发票与税务产品都查得的合并为「同时调用票税」，同一 (税号, 接口) 只取首次。
//  2. 用配置里的证通凭证直连重调：票调发票产品，税调税务产品，票税两个都调；以请求月为锚取
//     [请求月-24, 请求月] 窗口计算总金额。
//  3. 总金额 <= 0 不需向证通缴费；查无/失败列入「待确认」。
//  4. 按冠辰月度对账单格式出表：OUT_DIR/证通-月度对账单-<月>.xlsx。
//
// 可选环境变量：ISSUER（标题出单方，缺省 证通）、OUR_NAME（客户名称=我方）、OUT_DIR、WORKERS、LIMIT。
package main

import "github.com/datahub/relay/internal/tools/bill24"

func main() { bill24.Main(func() error { return bill24.RunUpstream("entcredit") }) }
