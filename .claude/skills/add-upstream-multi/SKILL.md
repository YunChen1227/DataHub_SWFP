---
name: add-upstream-multi
description: DataHub_SWFP 的多上游「按维度串行寻源 + 按实得内容计费」模型——新增/替换/调整数据源、改优先级、改成本、改收费档位、加新数据维度时必须使用本 skill。只要用户提到「加一个源」「换个源」「调优先级」「命中即停」「综合源」「缺项补齐」「按实得维度计费」「逐源对账」「上游成本」，即使没明说要用 skill，也必须使用本 skill——它固化了寻源引擎的语义边界、四类改动的最小清单与踩过的坑，照单执行，不要自行通读代码库。
---

# DataHub_SWFP 多源寻源与按实得计费

本服务只有一条对外路由（swfp），但内部有 N 个上游数据源。上游调用**不是**并发扇出，
而是**按优先级串行、命中即停**；计费**不看下游请求了什么，只看实际查得了什么**。
这两条是本仓与主仓 DataHub 最大的差别，改任何一个源之前必须先理解本文件。

参考实现：[internal/infrastructure/upstream/sourcing.go](internal/infrastructure/upstream/sourcing.go)（寻源引擎）、
[internal/domain/model/sourcing.go](internal/domain/model/sourcing.go)（维度/费率/轨迹类型）。

> **不适用**：若某条路由的多个源是「同一种数据、可互相替代」的纯备源关系（没有维度
> 概念、不存在组合交叉），那是主仓 DataHub 的 `add-upstream-multi` 模型，简单得多，
> 不要把本文件的维度/综合源/分档计费搬过去。

## 三个必须分清的概念

| 概念 | 类型 | 含义 | 谁决定 |
|---|---|---|---|
| **Want** | `model.DimSet` | 下游这次**要什么**（发票 / 税务 / 两者） | 入参 `dataType`，缺省 both |
| **Provides** | `model.DimSet` | 某个源**能提供什么** | config 的 `provides` |
| **Got** | `model.DimSet` | 本次**实际查得了什么** | 寻源结果累积 |

**计费只看 Got**：请求两项而只查得发票 → 业务码仍是 `001`，但按【单发票】档收费。
把 Want 当计费依据是本模型最容易犯的错——那会向客户收没给到的数据的钱。

## 寻源流程（代码即流程图，改动前对照一遍）

```
1. 解析入参 → Want（dataType: invoice|tax|both）+ 建全链路追踪表 trace
2. Want=两者 → 2b 遍历【综合源】列表（Provides 同时含发票+税务），两者皆得即停
   Want=单项 → 2a 直接遍历该维度列表，命中即停
3. 综合源用尽仍缺 → 缺项补齐：按缺失维度分别遍历发票源/税务源，
                    **过滤本次已请求过的逻辑源**
4. 汇总 Got
5. 按 Got 定档：invoice+tax / invoice / tax / none(不计费)
6. 返回下游（range 内仅业务数据 + dataScope；**无** 源N / sourceStatus / feeStandard）
7. 落库：台账(档位/应收/成本/逐源汇总) + 审计(请求维度/实得维度) + upstream_call(每源一行)
```

代码位置：`Sourcer.Query` 依次调 `traverse(combined)` → `traverse(invoice)` →
`traverse(tax)` → `fillSkipped` → `trace.result`。**2a 与 3 是同一段代码**
（单项寻源 = 候选集为空的缺项补齐），不要为它们写两套逻辑。

## 铁律（违反任何一条即返工）

1. **逻辑源 ≠ 一次调用，互补调用必须同源**。证通的发票聚合 part1/part2 各出一半
   字段，必须一起发出；它们同属逻辑源 `ent_invoice`（config 的 `source` 字段）。
   若把它们拆成两个逻辑源，「命中即停」会在 part1 命中后跳过 part2，**下游拿到的
   字段比改造前更少**——这是本模型最隐蔽的数据缺失事故。逻辑源内的多次调用由
   `Sourcer.invoke` 并发发出（同源同价，没有短路空间可省）。

2. **逻辑源名是去重键，一次请求内绝不重复调用同一个源**。综合源阶段调过的源，在
   缺项补齐阶段必须被 `trace.done[name]` 过滤掉——重复调用等于**重复付费**且拿不到
   新数据。新增源时若与既有源共用 `source` 名，务必确认它们真的是互补调用而非独立源。

3. **计费只看 Got**（见上）。`billing.Decide(result, lic.Rates)` 用
   `model.StandardOf(result.Got)` 定档，禁止引入 Want。

4. **上游成本无条件落库，哪怕本次不计费**。查无/全失败也已经花了钱，亏损单必须在
   库里看得见：失败路径的 `*model.UpstreamError` 同样带 `CostFen` 与 `Sources`，
   orchestrator 会落进 `upstream_cost_fen`。成本口径按源可配：`costOn: hit`（缺省，
   仅查得计费）/ `call`（调用即计费）——不同上游商务条款不同，不要写死。

5. **逐源留痕必须含未调用的源**。被更高优先级源短路掉的源要出一条 `status=skipped`
   的轨迹行并写明 `reason`。「没调哪些源、为什么没调」和「调了哪些源」同样是对账
   证据——客户质疑"为什么没查到"时，这是唯一能自证的材料。见 `trace.fillSkipped`。

6. **失败也要可追查**。子源"已应答但业务失败"时上游订单号/请求号在
   `*model.UpstreamError` 里（此时 `res` 为 nil，只从 `res` 取会全空）。必须
   `errors.As` 捞出 `Code/UID/LogID/Msg`；全部源失败时返回 `*model.UpstreamError`
   而非裸 `fmt.Errorf`，否则审计三列全空、无法向上游对账。

7. **总时延预算是硬闸门**。串行最坏情况是各源耗时相加，`upstream.budget`（缺省 9s）
   用尽后不再尝试下一个源，只记 `reason`。**禁止**为了多试一个源而放宽预算把下游
   拖到超时——省钱的前提是不违约。

8. **契约层严格白名单 + 对下游隐匿源**。只输出 `docs/税票分析接口文档.xlsx` 定义的
   字段；查得后把各源数据**扁平合并**再给下游（`nsrjbxx` 合成一个对象，各 List
   拼成一个数组）。响应里**不得**出现 `源N` / `sourceStatus` / `feeStandard`——
   下游不知道有几个源、也不知道数据从哪来。`SourceAlias` 仅供内部寻源轨迹 /
   `upstream_call` / 成本对账使用，不进契约输出。上游产品码/真实厂商/错误详情
   一律不得透出。新增契约字段前先问：这个字段会不会让下游数出我方有几个源。

9. **一源一文档，逐字对齐**。本仓的源来自不同厂商不同协议（源1-4 证通 entcredit：
   HMAC-SHA256 + form 表单 + 产品码；源5 凯盈云 crestv：AES + JSON 信封 + 接口名走
   URL 路径；源6 ctax：综合源）。**禁止**把已实现源的协议/凭证形态/加密方式套到新源上；
   新源的文档要完整读完（PDF 用 Read 工具整篇读，鉴权与错误码常在文档中后段），
   签名算法要拿到 SDK 源码而不是凭文档描述猜；上游服务器的报错信息比文档示例更权威。

## 判定表与计费档位

| 寻源结果 | body.code | 计费 | 台账 |
|---|---|---|---|
| 实得 ≥1 维度（含只得一半） | `001` | 按**实得维度**定档 | BILLED |
| 无实得 + ≥1 源失败 | `002` 未取得数据且部分数据源异常 | 否 | BILLED |
| 无实得 + 全部查无 | `999` | 否 | BILLED |
| 全部调用失败 | error → `505062` | 否 | PENDING → 复查/对账 |
| 本次维度无可用源（配置缺口） | error → `505062` | 否 | PENDING |

| 实得维度 | 台账 `fee_standard`（不对下游输出） | 单价来源（前者为 0 时取后者） |
|---|---|---|
| 发票 + 税务 | `both` | license `rate_both_fen` → config `billing.rates.bothFen` |
| 仅发票 | `invoice` | license `rate_invoice_fen` → config `billing.rates.invoiceFen` |
| 仅税务 | `tax` | license `rate_tax_fen` → config `billing.rates.taxFen` |
| 皆无 | `none` | 恒 0，不计费 |

> 下游 `result.range` 只带 `dataScope`（实得维度）与业务数据；`feeStandard` 只写
> 台账/审计，供我方对账，不进契约。

金额一律用「分」的整数（`AmountFen`/`CostFen`），**不用浮点**——对账要逐笔相加，
不能有舍入漂移。

## 四类改动（先判断属于哪一类，再照该类清单执行）

### A. 新增/替换一个「单维度」源（最常见，只改配置 + 契约映射）

寻源引擎对源数量完全泛化，**不需要改 Go 逻辑**：

1. **配置**：先改提交进仓的模板 `config.example.yaml`（占位符凭证），再改**每一份**
   实际使用的配置（`config.aliyun.prod.yaml` / `config.aliyun.e2e.yaml` /
   `config.local.mem.yaml`——这几份都已 gitignore，需提醒用户在本机/服务器上同步补），
   在 `versions.swfp.upstreams` 追加条目：
   `kind` / `label`(契约段名) / `source`(逻辑源名) / `provides`(invoice|tax) /
   `priority`(越小越先) / `costFen`(上游单价，分) / `costOn` / `optional` + 该源完整凭证。
   同一逻辑源的多次互补调用写多条、`source` 填同一个值。
2. **新 kind 才需要**新建 `internal/infrastructure/upstream/<kind>.go`（实现
   `port.UpstreamPort`，归一为 `001`/`999`/`*model.UpstreamError`），并在
   `cmd/relay/main.go` 的 `buildClient` 加一个 case、`config.go` 补凭证字段。
   复用既有 kind（如再加一个 entcredit 产品码）则**零 Go 改动**。
3. **契约层**（[swfpcontract.go](internal/infrastructure/upstream/swfpcontract.go)）：
   `swfpSourceAlias` 加 `label → 源N`（**内部**轨迹用），并在 `mapSwfpRange` 的
   `switch label` 里加该段的字段映射（严格白名单，逐字段标注依据 xlsx 哪一节）。
   填完后由 `flattenSegment` 压平成对下游形状——**漏了映射**该源的数据会被当
   "未知段"不透出，线上表现是"调了、计费了、但下游没数据"。
4. **单测**：见下「测试」。用真实上游/探针验证接入，**不要**为新源写 HTTP mock
   挡板或靠 mock 驱动的 e2e——那种测既测不到真实协议，又要维护一套假场景。

### B. 新增一个「综合源」（一次同时给发票+税务）

`provides: "both"` 即可，`NewSourcer` 会自动把它放进 `combined` 列表、流程自动走 2b。
首次接入综合源时：在 `sourcing_test` 用进程内 fakePort 断言「两项请求只调该源一次、
其余 skipped」；下游契约断言扁平 range + `dataScope` 两维皆真。再用真实凭证探针
打通一笔。其余同 A 类。

综合源有一个单维度源不会遇到的坑：**一次调用可能只拿到声明维度的一部分**。此时
客户端必须自报到 `UpstreamResult.Got`，寻源引擎优先采信自报值并继续补齐缺维。

### C. 只调优先级 / 成本 / 可选性

纯配置改动（`priority` / `costFen` / `costOn` / `optional`）。排序规则是
`(priority 升 → 逻辑源总成本升 → 配置顺序)`，未显式给 priority 时全为 0，自然退化为
「价格由低到高」。改完必须重跑寻源单测的「命中即停/回落」断言——优先级改动会直接
改变哪些源被 `skipped`，进而改变成本与轨迹。

### D. 新增一个数据维度（大改，牵动计费与库表）

维度是**两个 bool 而不是 map/bitmask**（`model.DimSet{Invoice, Tax}`）——这是有意的
简化取舍。加第三个维度要动的地方，一处不能漏：

1. [model/sourcing.go](internal/domain/model/sourcing.go)：`DimSet` 加字段并同步
   `NewDimSet`/`DimSetOf`/`Has`/`Empty`/`Both`/`Union`/`Intersect`/`Covers`/`Missing`/
   `String`；`DataType*` 常量；`FeeStandard` 档位与 `StandardOf`；`FeeRates` 档位与
   `Of`/`OrDefault`。**注意 `Both()` 的语义**是"综合源判定"，加维度后要重新定义
   "综合源"是"覆盖全部维度"还是"覆盖 ≥2 个维度"，并同步 `NewSourcer` 的分列表逻辑
   与 `Sourcer.Query` 的遍历顺序。
2. [parse.go](internal/domain/parse/parse.go)：`ParseCreditCode` 的 `dataType` 白名单。
3. [billing.go](internal/domain/billing/billing.go)：新档位的费率与金额判定。
4. [swfpcontract.go](internal/infrastructure/upstream/swfpcontract.go)：`dataScope`
   输出与新段的白名单映射。
5. **库表**：新 migration（照 `migrations/0007_sourcing_billing.sql` 的形状）加
   license 的档位费率列、`upstream_call` 的维度列；pg 与 memory 两套 store 同步改。
6. 后台：`Users.jsx` 费率列/编辑项、`Audits.jsx` 维度列。

## 测试（改完必须全绿；不要写/扩 mock 挡板）

**不要**新增或维护 `scripts/mock_*.go`、也不要靠 mock 驱动的 `test/cases` /
`test/run.ps1` 当验收门槛——它们既测不到真实协议与鉴权，又要维护一套与线上无关
的假场景，投入产出不成正比。接入验证用真实上游探针（如既有 `scripts/probe_*.go`）。

必跑的是进程内单测（fakePort，不是 HTTP mock）：

1. **寻源引擎** [sourcing_test.go](internal/infrastructure/upstream/sourcing_test.go)：
   新增源/改优先级后至少补两条断言——**该源在什么条件下被调用**、**在什么条件下被
   `skipped`**。命中即停 / 回落 / 综合源短路等**逐源状态只在此断言**（或后台
   `upstream_call`），不得再从下游 `result.range` 读 sourceStatus。
2. **计费** [billing_test.go](internal/domain/billing/billing_test.go)：按 Got
   定档、license 费率逐档覆盖全局缺省、不计费时成本照样带出。
3. **契约** [swfpcontract_test.go](internal/infrastructure/upstream/swfpcontract_test.go)：
   扁平输出、白名单、无 `源N`/`sourceStatus`/`feeStandard` 泄漏、`dataScope` 正确。
4. **全量单测**：`go build ./...`、`go vet ./...`、`go test -count=1 ./internal/...`。

## 交付前自检（四层齐了才算完）

对**每一个**源逐层核对，缺一层就是线上"调不通"或"计费不对"：

1. 该源自己的文档 → 2. 该源的 client（文件头注释写明依据哪份文档）→
3. 真实配置的 `upstreams` 条目（凭证是真值而非占位符，`source`/`provides`/`priority`/
`costFen` 齐全）→ 4. 契约层的段名映射与字段白名单。

另外确认：**费率是否已填真值**——`billing.rates` 与 license 三档若全为 0，
`amount_fen` 会恒为 0（计数照常、金额算不出来），上线前需商务确认合同价。
`costFen` 全为 0 时成本对账同样无意义。

## 上线注意

- `totalCalls`（调用上游次数）口径 = **下游一次查询计 1 次**，不按源乘 N；
  逐源明细在 `upstream_call` 表，后台按 requestId 下钻
  （`GET /admin/api/{ver}/audits/{requestId}/calls`）。
- 新 migration 由 relay 启动时自动执行；`upstream_call` **无历史回填**，改造前的
  请求下钻会返回空列表，这是已知且接受的。
- 对外手册（api-doc skill）需同步：`dataType` 入参、扁平 `result.range`（两段业务
  数据 + `dataScope`，**无**源编号/`sourceStatus`/`feeStandard`）、`002` 的语义
  （**未取得数据**且部分数据源异常；取得了数据即 `001` 并计费）、以及分档计费口径
  （计费档位只在我方台账）。措辞仍受「上游隐匿」铁律约束——不得暴露真实厂商与产品码。
