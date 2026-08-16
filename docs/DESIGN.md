# DataHub_SWFP 设计说明

## 1. 定位

本服务是 **SWFP 税务发票聚合** 的 API 转接网关：

- **对外**：统一信封（MD5 加签、`head/body` 响应），路由 `querySrmxSWFP` / `quotaSWFP`
- **对内**：按优先级**串行寻源**（命中即停）调用证通 entcredit（发票/税务四产品码）+ 可选 salesdata（源5 销项），经 `SwfpContract` 按 `docs/税票分析接口文档.xlsx` 整理输出

## 2. 入参

| 字段 | 必填 | 说明 |
|------|------|------|
| creditCode | 是 | 统一社会信用代码（18 位） |
| dataType | 否 | `both`（默认，发票+税务）/ `invoice`（仅发票）/ `tax`（仅税务） |
| scope | 否 | `all`（默认，含源5）或 `basic`（仅源1-4） |

校验：`internal/domain/parse/ParseCreditCode`。`dataType` 决定**请求维度**（`model.DimSet`），
不带该入参的老下游按 `both` 处理，行为与改造前一致。

## 3. 串行寻源

`internal/infrastructure/upstream/sourcing.go` 的 `Sourcer` 取代了原并发聚合器。
配置里每个 `upstreams` 条目是一次**调用**，`source` 相同的调用组成一个**逻辑源**
（part1/part2 是互补字段，必须一起发出，故同属一个逻辑源并发调用；逻辑源之间才串行）。

```
Client → Relay → Sourcer
   want=both  ├─ 2b 综合源列表（provides=发票+税务，当前配置为空）→ 两者皆得即停
              ├─ 3  缺发票 → 遍历发票源 [ent_invoice(p1) → sales(p9)]，过滤已调用源
              └─ 3  缺税务 → 遍历税务源 [ent_tax(p1)]，过滤已调用源
   want=单项  └─ 2a 直接遍历该维度列表，命中即停
                 → SwfpContract → 下游 JSON
```

关键约束：

- **命中即停**：某维度已查得后，该维度更低优先级的源不再调用（省钱），轨迹里记为 `skipped`。
- **不重复付费**：逻辑源名是去重键，综合源阶段调过的源在补齐阶段被过滤。
- **总时延预算**：`upstream.budget`（缺省 9s）是本次全部上游调用的合计闸门；预算耗尽
  不再尝试下一个源并记录原因，避免串行把下游拖到超时。
- **成本口径**：每次调用的 `costFen` 按源配置，`costOn=hit`（缺省，仅查得计费）
  或 `call`（调用即计费），随各上游商务条款。

判定表（`trace.result`）：

| 结果 | body.code | 下游计费 | 台账 |
|------|-----------|----------|------|
| 实得 ≥1 维度（含只得一半） | 001 | 是，按**实得维度**定档 | BILLED |
| 无实得 + 有源失败 | 002 | 否 | BILLED |
| 无实得 + 全部查无 | 999 | 否 | BILLED |
| 全部失败 | 505062 | 否 | PENDING → 复查/对账 |

## 3.1 响应中的寻源信息

`result.range` 除各源数据外额外给出三个字段，使下游能自查本次计费依据：

| 字段 | 说明 |
|------|------|
| `sourceStatus` | 各源本次状态：`ok` / `empty` / `error` / `skipped` |
| `dataScope` | 实际查得维度 `{"发票": bool, "税务": bool}` |
| `feeStandard` | 据实得维度判定的收费档位 `both` / `invoice` / `tax` / `none` |

## 4. 存储

单域 `swfp`：独立 PostgreSQL + Redis。License、台账、配额、审计均带 `route=swfp`。

管理后台控制面与 JWT 校验走 `swfp` 域 admin 服务。

## 5. 计费与台账

- 幂等键：`(app_key, version, reqid)`
- 成功查得（busiCode 10）计入 `serviceUsed`
- 每次调用上游计入 `totalCalls`
- PENDING 台账由 RequeryWorker 复查

### 5.1 按实得维度定档

收费标准只看**实际查得了什么**，与请求了什么无关（请求两项只查得发票 → 按单发票收）：

| 实得维度 | 收费标准 | 单价来源 |
|----------|----------|----------|
| 发票 + 税务 | `both` | license `rate_both_fen`，为 0 时取 config `billing.rates.bothFen` |
| 仅发票 | `invoice` | license `rate_invoice_fen` → config `billing.rates.invoiceFen` |
| 仅税务 | `tax` | license `rate_tax_fen` → config `billing.rates.taxFen` |
| 皆无 | `none` | 不计费，金额恒为 0 |

金额单位一律「分」（整数），避免累计对账的浮点漂移。客户合同价挂 license
（后台用户页可编辑，留空走全局缺省），上游成本挂源配置。

### 5.2 对账落库

| 表 | 粒度 | 新增内容 |
|----|------|----------|
| `billing_ledger` | 一次下游请求一行 | `fee_standard` / `amount_fen`（应收）/ `upstream_cost_fen`（成本）/ `source_total`·`source_ok`·`source_err` |
| `audit_log` | 一次下游请求一行 | `req_scope`（请求维度）/ `data_scope`（实得维度）/ `fee_standard` / `amount_fen` / `upstream_cost_fen` |
| `upstream_call` | **每源一行**（含 skipped） | 新表：逐源的状态/上游订单号/请求号/成本/耗时/跳过原因 |

`upstream_call` 用业务键 `(app_key, version, reqid, source_label)` 唯一约束，
重放幂等（`ON CONFLICT DO NOTHING`），并冗余 `request_id` 供后台按 requestId 下钻。
复查路径（`FromRequery`）只改状态与计数，不覆盖已定档的收费标准与金额。

迁移：`migrations/0007_sourcing_billing.sql`

## 6. 管理后台

- 登录：`POST /admin/api/login`
- 用户 CRUD：`/admin/api/swfp/users`
- 审计：`GET /admin/api/swfp/audits`（creditCode 脱敏存于 `idCardMask` 字段）
- 逐源明细：`GET /admin/api/swfp/audits/{requestId}/calls`（审计列表「逐源明细」下钻）
- 合同价：用户编辑弹窗内按「元」维护三档费率，留空表示走全局缺省

## 7. 相关文档

- 下游契约：`docs/税票分析接口文档.xlsx`
- 客户手册：`docs/API_接口文档与使用手册_swfp.pdf`
- 上游：发票/税务 part PDF、销项 `docs/销项数据接口文档V1.0.docx`
- 多源计费：`docs/设计_多源计费与上游对账.md`
