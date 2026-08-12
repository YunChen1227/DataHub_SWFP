# DataHub_SWFP

SWFP（税务发票聚合）API 转接服务。对外提供统一网关信封（`appKey/sign/encryptionType/body` + MD5 加签），对内并发聚合 4 路证通 entcredit 产品码 + 可选源5 销项数据（salesdata），输出按 `docs/税票分析接口文档.xlsx` 契约整理。

## 路由

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/v1/openapi/zlx/querySrmxSWFP` | 主查询（入参 `creditCode`，可选 `scope=all\|basic`） |
| GET | `/v1/openapi/zlx/quotaSWFP` | 配额/统计 |
| GET | `/healthz` | 健康检查 |
| POST | `/admin/api/login` | 管理后台登录 |
| * | `/admin/api/swfp/*` | 用户与审计管理 |

## 快速启动

```powershell
cd c:\workspace\DataHub_SWFP
cp config.example.yaml config.yaml   # 填入凭证
go run ./cmd/relay
```

本地全链路测试（memory + mock 上游）：

```powershell
# 终端1: mock entcredit (:9116)
go run ./scripts/mock_entcredit.go
# 终端2: mock salesdata (:9121)
go run ./scripts/mock_salesdata.go
# 终端3: relay
$env:CONFIG_FILE = "config.local.mem.yaml"; go run ./cmd/relay
# 终端4: 测试套件
powershell -ExecutionPolicy Bypass -File .\test\run.ps1 -ConfigFile config.local.mem.yaml
```

## 配置

- `config.example.yaml` — 配置模板（仅 `swfp` 块）
- `config.local.mem.yaml` — 本地 memory 模式
- `config.aliyun.prod.yaml` / `config.aliyun.e2e.yaml` — 生产/e2e（PostgreSQL + Redis）

Demo license（memory / SEED_DEMO=1）：

| 域 | appKey | secret |
|----|--------|--------|
| swfp | `y890swfp` | `demo-app-secret` |

## 目录结构

```
cmd/relay/          主程序入口
internal/
  api/              HTTP 路由与 admin API
  application/      编排、异步记账
  domain/           领域模型、鉴权、计费、解析
  infrastructure/upstream/  entcredit + salesdata + 聚合/契约层
scripts/            mock 上游、探测脚本、建库脚本
test/cases/         固定测试用例
web/admin/          管理后台 SPA
docs/               SWFP 上下游文档与契约
```

## 上游子源

| 段名 | kind | 产品/说明 |
|------|------|-----------|
| invoice1 | entcredit | P0130081 发票 part1 |
| invoice2 | entcredit | P0130083 发票 part2 |
| tax1 | entcredit | P0130082 税务 part1 |
| tax2 | entcredit | P0130084 税务 part2 |
| sales | salesdata | 源5 销项（optional，`scope=basic` 时跳过） |

详细设计见 `docs/DESIGN.md`。
