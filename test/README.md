# DataHub_SWFP 固定测试套件

一键运行 SWFP 全链路测试：启动 mock 上游 + relay，依次执行 `test/cases/` 下用例，结果写入 `test_res/<日期>/REPORT.md`。

## 运行

```powershell
powershell -ExecutionPolicy Bypass -File .\test\run.ps1
powershell -ExecutionPolicy Bypass -File .\test\run.ps1 -ConfigFile config.local.mem.yaml
```

## 用例

| 脚本 | 说明 |
|------|------|
| `00_connectivity.go` | PostgreSQL + Redis 连通性（postgres 模式） |
| `01_health_routes.go` | `/healthz` 与 SWFP query/quota 路由可达 |
| `06_admin_crud.go` | 管理后台登录、用户 CRUD、密钥轮换、审计 |
| `12_swfp_query.go` | SWFP 主接口全场景（五源聚合、scope=basic、查无/部分失败等） |

## Mock 上游

| 服务 | 端口 | 脚本 |
|------|------|------|
| entcredit（源1-4） | :9116 | `scripts/mock_entcredit.go` |
| salesdata（源5） | :9121 | `scripts/mock_salesdata.go` |

Demo 凭证：`appKey=y890swfp`，`secret=demo-app-secret`。
