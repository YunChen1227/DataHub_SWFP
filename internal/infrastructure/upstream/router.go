package upstream

// Provider identifiers select the upstream client family per 子源 (DESIGN §6)。
// 每条路由的上游是一个 Aggregator (aggregate.go)：单源直通、多源 (swfp) 并发聚合；
// 装配层 (cmd/relay) 按 kind 用 buildClient 构建每个子源 client。
const (
	ProviderEntCredit = "entcredit" // swfp: 税务+发票四产品码聚合
	ProviderSalesData = "salesdata" // swfp 第五子源: 销项数据 (凯盈云 crestv)
)
