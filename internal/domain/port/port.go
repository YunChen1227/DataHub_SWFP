// Package port declares the outbound interfaces (hexagonal "ports") the domain
// depends on. Infrastructure adapters implement them; the domain never imports
// infrastructure, keeping the dependency arrow pointing inward.
package port

import (
	"context"

	"github.com/datahub/relay/internal/domain/model"
)

// LicenseRepository loads license/identity rows (DESIGN §11.1).
type LicenseRepository interface {
	FindByAppKey(ctx context.Context, appKey string) (*model.LicenseView, error)
}

// QuotaRepository tracks per-route 统计 (DESIGN §7). 无额度限制，仅累计计数。
// 计数按 (licenseID, route) 独立：共享同一 license 的 v8/v9 各自独立统计。
// 所有累加必须原子，避免并发漏计。route = 路由名 (x1/v9/v8/zlf/blk)。
type QuotaRepository interface {
	// ServiceUsed returns the cumulative 成功查得数 (busiCode 10) for (license, route).
	ServiceUsed(ctx context.Context, licenseID, route string) (used int64, err error)
	// IncServiceUsed increments the 成功查得数 by 1 for (license, route).
	IncServiceUsed(ctx context.Context, licenseID, route string) error
	// TotalCalls returns the cumulative 调用上游次数 (CalledUpstream) for (license, route).
	TotalCalls(ctx context.Context, licenseID, route string) (calls int64, err error)
	// IncTotalCalls increments the 调用上游次数 by 1 for (license, route).
	IncTotalCalls(ctx context.Context, licenseID, route string) error
}

// LedgerRepository is the append-only billing台账 store (DESIGN §11.3).
type LedgerRepository interface {
	// FindByReqid returns the ledger for (appKey, route, reqid) or (nil, nil) if
	// absent. route 隔离共享 license 的 v8/v9 幂等键。
	FindByReqid(ctx context.Context, appKey, route, reqid string) (*model.Ledger, error)
	// Append inserts a new PENDING ledger and back-fills the assigned ID.
	Append(ctx context.Context, l *model.Ledger) error
	// Settle 终态化台账：状态 + 成功查得标记 + 计费标准/金额 + 上游成本与代表标识。
	Settle(ctx context.Context, id int64, s model.LedgerSettlement) error
	// ListByState powers the re-query worker and reconciliation job.
	ListByState(ctx context.Context, state model.BillingState, limit int) ([]*model.Ledger, error)
}

// UpstreamCallRepository 存「每个上游子源一行」的调用明细：一次 swfp 查询产生多行,
// 各带自己的上游订单号/请求号与成本，供向各家上游分别对账（1:N 关系装不进台账的
// 一对上游标识列，必须独立子表——设计_多源计费与上游对账 §2.1）。
type UpstreamCallRepository interface {
	AppendUpstreamCalls(ctx context.Context, calls []*model.UpstreamCallRecord) error
	ListUpstreamCalls(ctx context.Context, f model.UpstreamCallFilter) ([]*model.UpstreamCallRecord, error)
}

// UpstreamPort talks to a data provider (DESIGN §6). The active provider is
// selected by the upstream Router (当前唯一伽马); each provider normalizes
// its native response into model.UpstreamResult.
type UpstreamPort interface {
	Query(ctx context.Context, req *model.UpstreamRequest) (*model.UpstreamResult, error)
	// Requery is the idempotent re-query by reqid (never double-charges).
	Requery(ctx context.Context, reqid string) (*model.RequeryResult, error)
}

// SecretProvider supplies the客户下游 MD5 secret from KMS/Vault (DESIGN §11.4);
// never logged. Upstream provider credentials are injected via process config.
type SecretProvider interface {
	AppSecret(ctx context.Context, licenseID string) (string, error)
}

// SignatureVerifier validates the client MD5 signature (DESIGN §8.1 / PDF §3.1).
type SignatureVerifier interface {
	Verify(req *model.SignedRequest, appSecret string) bool
}

// --- Admin console ports (DESIGN §16) ---

// AdminUserRepository stores operator accounts (DESIGN §16.1).
type AdminUserRepository interface {
	FindAdmin(ctx context.Context, username string) (*model.AdminUser, error)
	PutAdmin(ctx context.Context, a *model.AdminUser) error
}

// UserAdminRepository manages普通用户 (license) lifecycle + bound secret for the
// admin console (DESIGN §16.2). v0.7 起携带手机号；IP 准入交由阿里云 ECS 安全组。
type UserAdminRepository interface {
	// route 决定统计 (成功查得数/调用次数) 的作用域：共享 license 的 v8/v9 列表相同，
	// 但各用户的统计随 route 不同。license 本身 (CRUD) 与 route 无关。
	ListUsers(ctx context.Context, route string) ([]*model.UserDetail, error)
	// SearchUsers 按关键字匹配 appKey / 名称 / 手机号 (任一包含即命中)。
	SearchUsers(ctx context.Context, keyword, route string) ([]*model.UserDetail, error)
	GetUser(ctx context.Context, licenseID, route string) (*model.UserDetail, error)
	// CreateUser persists a new license + quota + bound secret (plaintext secret
	// is passed in; the adapter is responsible for at-rest encryption, §11.4).
	CreateUser(ctx context.Context, d *model.UserDetail, secret string) error
	UpdateUser(ctx context.Context, licenseID string, status string, mobile string) error
	// SetRates 更新该客户的三档合同价（单位分；某档 0 = 走全局缺省费率）。
	SetRates(ctx context.Context, licenseID string, rates model.FeeRates) error
	DeleteUser(ctx context.Context, licenseID string) error
	RotateSecret(ctx context.Context, licenseID, secret string) error
}

// AuditRepository is the append-only audit log store (DESIGN §16.3/§16.5).
type AuditRepository interface {
	AppendAudit(ctx context.Context, rec *model.AuditRecord) error
	ListAudits(ctx context.Context, f model.AuditFilter) ([]*model.AuditRecord, error)
}
