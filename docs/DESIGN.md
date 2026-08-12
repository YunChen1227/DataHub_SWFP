# DataHub_SWFP 设计说明

## 1. 定位

本服务是 **SWFP 税务发票聚合** 的 API 转接网关：

- **对外**：统一信封（MD5 加签、`head/body` 响应），路由 `querySrmxSWFP` / `quotaSWFP`
- **对内**：并发调用 4 路证通 entcredit（发票/税务四产品码）+ 可选 salesdata（源5 销项），经 `SwfpContract` 按 `docs/税票分析接口文档.xlsx` 整理输出

## 2. 入参

| 字段 | 必填 | 说明 |
|------|------|------|
| creditCode | 是 | 统一社会信用代码（18 位） |
| scope | 否 | `all`（默认，含源5）或 `basic`（仅源1-4） |

校验：`internal/domain/parse/ParseCreditCode`

## 3. 上游聚合

```
Client → Relay → Aggregator
                    ├─ entcredit (invoice1, P0130081)
                    ├─ entcredit (invoice2, P0130083)
                    ├─ entcredit (tax1, P0130082)
                    ├─ entcredit (tax2, P0130084)
                    └─ salesdata (sales, optional)
                 → SwfpContract → 下游 JSON
```

判定（`internal/infrastructure/upstream/aggregate.go`）：

| 结果 | body.code | 计费 |
|------|-----------|------|
| ≥1 查得 | 001 | 是 |
| 全查无 | 999 | 否 |
| 部分源失败 | 002 | 否 |
| 全失败 | 505062 | 否 |

## 4. 存储

单域 `swfp`：独立 PostgreSQL + Redis。License、台账、配额、审计均带 `route=swfp`。

管理后台控制面与 JWT 校验走 `swfp` 域 admin 服务。

## 5. 计费与台账

- 幂等键：`(app_key, version, reqid)`
- 成功查得（busiCode 10）计入 `serviceUsed`
- 每次调用上游计入 `totalCalls`
- PENDING 台账由 RequeryWorker 复查

## 6. 管理后台

- 登录：`POST /admin/api/login`
- 用户 CRUD：`/admin/api/swfp/users`
- 审计：`GET /admin/api/swfp/audits`（creditCode 脱敏存于 `idCardMask` 字段）

## 7. 相关文档

- 下游契约：`docs/税票分析接口文档.xlsx`
- 客户手册：`docs/API_接口文档与使用手册_swfp.pdf`
- 上游：发票/税务 part PDF、销项 `docs/销项数据接口文档V1.0.docx`
- 多源计费：`docs/设计_多源计费与上游对账.md`
