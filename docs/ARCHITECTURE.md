# DataHub_SWFP 架构

```
                    ┌─────────────────┐
                    │  Client / Admin │
                    └────────┬────────┘
                             │ HTTP
                    ┌────────▼────────┐
                    │   cmd/relay     │
                    │  api/handler    │
                    └────────┬────────┘
         ┌───────────────────┼───────────────────┐
         │                   │                   │
  ┌──────▼──────┐    ┌───────▼───────┐   ┌──────▼──────┐
  │ auth/quota  │    │ orchestrator  │   │ admin SPA   │
  │ billing     │    │ + bookkeeper  │   │ (swfp)      │
  └──────┬──────┘    └───────┬───────┘   └─────────────┘
         │                   │
  ┌──────▼──────┐    ┌───────▼───────────────────────────┐
  │ PG + Redis  │    │ upstream.Aggregator               │
  │ (swfp 域)   │    │  → entcredit ×4 + salesdata ×1    │
  └─────────────┘    │  → SwfpContract                   │
                     └───────┬───────────────────────────┘
                             │
              ┌──────────────┼──────────────┐
              │              │              │
       证通 entcredit   凯盈云 salesdata   (mock :9116/:9121)
```

## 分层

| 层 | 包 | 职责 |
|----|-----|------|
| 入口 | `cmd/relay`, `internal/api` | 配置加载、HTTP 路由、admin API |
| 应用 | `internal/application` | 请求编排、异步记账、复查 worker |
| 领域 | `internal/domain` | 鉴权、计费、配额、解析、模型 |
| 基础设施 | `internal/infrastructure` | PG/Redis 持久化、上游客户端 |

## 部署拓扑

- 单实例 relay 监听 `:8080`（可配置）
- 生产：`storage.driver=postgres`，`datahub_swfp_db` + Redis db5
- 开发：`storage.driver=memory`，无需外部依赖
