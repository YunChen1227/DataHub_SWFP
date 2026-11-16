//go:build ignore

// bill_upstream_shuiyou_24m: 税友（provider=sy）24 个月口径月度对账单。
// 必须在出口 IP 已进税友白名单的阿里云 ECS 上运行。
//
//	CONFIG_FILE=config.aliyun.prod.yaml MONTHS=2026-09,2026-10 go run ./scripts/bill_upstream_shuiyou_24m.go
//
// 步骤：
//  1. 直接查库：upstream_call 当月 provider=sy 且 status=ok 的调用（不分下游客户），
//     与请求的 req_scope 取交集得出 只调用票 / 只调用税 / 同时调用票税，同一 (税号, 接口) 只取首次。
//  2. 用配置里的税友凭证直连重调，authTimeEnd 取当时的请求日期；以请求月为锚取
//     [请求月-24, 请求月] 窗口计算总金额。
//  3. 总金额 <= 0 不需向税友缴费；查无/失败列入「待确认」。
//  4. 按冠辰月度对账单格式出表：OUT_DIR/税友-月度对账单-<月>.xlsx。
//
// 可选环境变量：ISSUER（标题出单方，缺省 税友）、OUR_NAME（客户名称=我方）、OUT_DIR、WORKERS、LIMIT。
package main

import "github.com/datahub/relay/internal/tools/bill24"

func main() { bill24.Main(func() error { return bill24.RunUpstream("sy") }) }
