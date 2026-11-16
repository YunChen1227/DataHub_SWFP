//go:build ignore

// bill_chenyun_new_found: chenyun_dev_new 这个下游调用方的查得口径月度对账单
// （只读库，不重调、不判金额）。用于你自己发起一批调用后，把查得的数据抓出来核对。
//
//	CONFIG_FILE=config.aliyun.prod.yaml MONTHS=2026-09,2026-10 go run ./scripts/bill_chenyun_new_found.go
//
// 步骤：
//  1. 直接查库：license.name 模糊匹配或 app_key 精确等于 chenyun_dev_new 的调用方，
//     取当月 found_data 的请求，按 req_scope 分 只调用票 / 只调用税 / 同时调用票税，
//     记录税号与请求时间。
//  2. 查得即计费，按冠辰月度对账单格式出表，9 月 / 10 月各一个文件：
//     OUT_DIR/chenyun_dev_new-月度对账单-查得口径-<月>.xlsx（文件名前缀取库里的展示名）。
//
// 可选环境变量：CUSTOMER（缺省 chenyun_dev_new，可改成其它调用方的 name 或 app_key）、
// ISSUER（标题出单方）、OUT_DIR（缺省 test_res/bill_found）、
// DEDUPE（缺省 1：同月同 (税号, 接口) 只计首次；0：每次调用都列出）。
package main

import (
	"os"

	"github.com/datahub/relay/internal/tools/bill24"
)

func main() {
	if os.Getenv("CUSTOMER") == "" {
		os.Setenv("CUSTOMER", "chenyun_dev_new")
	}
	bill24.Main(bill24.RunDownstreamFound)
}
