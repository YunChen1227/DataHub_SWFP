// Package application wires the domain services into the主调用流程 (DESIGN §4).
// It owns transaction/flow orchestration only — no business rules live here.
package application

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/datahub/relay/internal/common/appctx"
	"github.com/datahub/relay/internal/common/errs"
	"github.com/datahub/relay/internal/common/mask"
	"github.com/datahub/relay/internal/domain/auth"
	"github.com/datahub/relay/internal/domain/billing"
	"github.com/datahub/relay/internal/domain/mapping"
	"github.com/datahub/relay/internal/domain/model"
	"github.com/datahub/relay/internal/domain/parse"
	"github.com/datahub/relay/internal/domain/port"
	"github.com/datahub/relay/internal/domain/quota"
)

// QueryOrchestrator implements the §4 sequence. route 标记本编排器服务的路由
// (x1/v9/v8/zlf/blk/swfp)，用于把统计/台账/审计按路由作用域隔离 (共享 license 的 v8/v9)。
type QueryOrchestrator struct {
	route    string
	auth     *auth.Service
	quota    *quota.Service
	billing  *billing.Service
	upstream port.UpstreamPort
	audit    port.AuditRepository
	books    *Bookkeeper // 异步记账（结算+审计）；nil 时退化为同步（测试/未装配）
	parseFn  func(*model.QueryCommand) (*model.UpstreamRequest, error)
	log      *slog.Logger
}

func NewQueryOrchestrator(route string, a *auth.Service, q *quota.Service, b *billing.Service, up port.UpstreamPort, audit port.AuditRepository, log *slog.Logger) *QueryOrchestrator {
	if log == nil {
		log = slog.Default()
	}
	return &QueryOrchestrator{route: route, auth: a, quota: q, billing: b, upstream: up, audit: audit, parseFn: parse.ParseCreditCode, log: log}
}

// WithParser replaces the parameter validator (default ParseCreditCode for SWFP).
func (o *QueryOrchestrator) WithParser(fn func(*model.QueryCommand) (*model.UpstreamRequest, error)) *QueryOrchestrator {
	if fn != nil {
		o.parseFn = fn
	}
	return o
}

// WithBookkeeper 挂接异步记账器：结算 + 审计移出响应关键路径（每请求省 3-5 次
// 串行 DB 写）。未挂接时保持旧行为（同步落库）。
func (o *QueryOrchestrator) WithBookkeeper(b *Bookkeeper) *QueryOrchestrator {
	o.books = b
	return o
}

// Handle runs the full request lifecycle and returns a ready-to-serialize
// QueryResponse (接口文档-经济能力.doc head/body). 网关级失败落在 head.errorCode;
// 查得/查无落在 body.code (001/999). A rich audit record (DESIGN §16.3) is
// written for every request via a deferred hook.
func (o *QueryOrchestrator) Handle(ctx context.Context, signed *model.SignedRequest, cmd *model.QueryCommand) *model.QueryResponse {
	requestID := appctx.RequestID(ctx)
	clientIP := appctx.ClientIP(ctx)
	start := time.Now()
	log := o.log.With("requestId", requestID, "clientIp", clientIP)
	lat := func() int64 { return time.Since(start).Milliseconds() }

	rec := &model.AuditRecord{
		RequestID:  requestID,
		Version:    o.route,
		AppKey:     signed.AppKey,
		ClientIP:   clientIP,
		IDCardMask: mask.CreditCode(cmd.CreditCode),
	}
	// 结算 + 审计在响应构造完成后统一提交（异步记账，见 Bookkeeper）。settleTok/
	// settleDec 由 runCore 在拿到上游确定结论时填入；PENDING/失败路径保持 nil。
	var settleTok *quota.ReserveToken
	var settleDec *model.BillingDecision
	defer func() {
		rec.FoundData = rec.BusiCode == int(errs.BusiSuccess)
		rec.LatencyMs = lat()
		rec.CreatedAt = time.Now()
		o.submitBooks(settleTok, settleDec, rec, log)
	}()

	fail := func(busi errs.BusiCode, msg string) *model.QueryResponse {
		rec.BusiCode = int(busi)
		rec.BusiMsg = msg
		return mapping.Error(busi, msg, requestID, lat())
	}

	// 1. License + appKey + signature.
	lic, err := o.auth.Authenticate(ctx, signed)
	if err != nil {
		ae := errs.AsAppError(err)
		rec.ErrMsg = ae.Error()
		log.Warn("auth failed", "busiCode", ae.Busi, "err", err)
		return fail(ae.Busi, ae.Msg)
	}
	log = log.With("appKey", lic.AppKey)

	// 2. 无额度限制：不做余额拦截，仅在查得数据时累计成功查得数 (见 Settle)。

	// 3. Param validation + build upstream request (我方拦截, before reserve).
	upReq, err := o.parseFn(cmd)
	if err != nil {
		ae := errs.AsAppError(err)
		rec.ErrMsg = ae.Error()
		log.Info("param invalid", "err", err)
		return fail(ae.Busi, ae.Msg)
	}
	rec.Reqid = upReq.Reqid
	log = log.With("reqid", upReq.Reqid)

	// 4-6. Idempotency + reserve + upstream (settle 移交异步记账).
	out := o.runCore(ctx, lic, upReq, requestID, rec, log)
	settleTok, settleDec = out.settleTok, out.settleDec
	return o.respondX1(out, requestID, rec, lat())
}

// submitBooks 提交记账任务：装配了 Bookkeeper 时异步（入队即返回）；否则同步
// 落库（保持旧行为，供测试与未装配场景）。同步路径用独立 ctx——本方法在响应
// 即将写回时执行，请求 ctx 生命周期已不可依赖。
func (o *QueryOrchestrator) submitBooks(tok *quota.ReserveToken, dec *model.BillingDecision, rec *model.AuditRecord, log *slog.Logger) {
	if o.books != nil {
		o.books.Submit(bookTask{token: tok, decision: dec, rec: rec})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if tok != nil && dec != nil {
		if err := o.quota.Settle(ctx, tok, dec); err != nil {
			log.Error("settle failed", "err", err)
		}
	}
	if o.audit != nil {
		if err := o.audit.AppendAudit(ctx, rec); err != nil {
			log.Error("append audit failed", "err", err)
		}
	}
}

// queryOutcome is the normalized result of the post-auth core flow, shared by
// the x1 (head/body) and v9 (income_cls.md) response mappers.
type queryOutcome struct {
	decision *model.BillingDecision // settled verdict (查得/查无/未扣费)
	existing *model.Ledger          // idempotent hit (already BILLED)
	appErr   *errs.AppError         // reserve/upstream-unresolved failure
	// settleTok/settleDec 是移交异步记账的结算工作单（上游给出确定结论时成对
	// 填入；PENDING/重放/失败路径为 nil，台账留待复查/对账）。
	settleTok *quota.ReserveToken
	settleDec *model.BillingDecision
}

// runCore runs the shared §4 steps after authentication: 幂等命中、开台账、上游
// 调用(+按 reqid 复查)。It updates the audit record's flow fields; settlement is
// handed off to the async Bookkeeper (settleTok/settleDec)；wire-format mapping
// is left to the caller.
func (o *QueryOrchestrator) runCore(ctx context.Context, lic *model.LicenseView, upReq *model.UpstreamRequest, requestID string, rec *model.AuditRecord, log *slog.Logger) queryOutcome {
	// reqidIsFresh=true：reqid 由本次请求内部新生成（parse.NewReqid），幂等查询
	// 必 miss，跳过该次 DB 读（关键路径优化，见 quota.Begin 注释）。
	token, existing, err := o.quota.Begin(ctx, lic, o.route, upReq.Reqid, "", requestID, true)
	if err != nil {
		ae := errs.AsAppError(err)
		rec.ErrMsg = ae.Error()
		log.Info("begin ledger failed", "busiCode", ae.Busi)
		return queryOutcome{appErr: ae}
	}
	if existing != nil {
		log.Info("idempotent hit, replaying cached billed result")
		rec.CalledUpstream = true
		rec.Billed = existing.CountedService
		return queryOutcome{existing: existing}
	}

	result, callErr := o.upstream.Query(ctx, upReq)
	var decision *model.BillingDecision
	if callErr != nil {
		// 失败也全量落审计：若上游"已应答但以业务码拒绝"(model.UpstreamError)，
		// 把上游返回的 code/uid(订单号)/logId(请求号) 记进审计，便于向上游对账追查
		// (即便随后 PENDING)。纯网络不可达没有这些标识，仅记 ErrMsg。
		var ue *model.UpstreamError
		if errors.As(callErr, &ue) {
			rec.CalledUpstream = true // 上游确已应答(业务失败)，属"已调用上游"
			rec.UpstreamCode = ue.Code
			rec.UpstreamUID = ue.UID
			rec.UpstreamLogID = ue.LogID
		}
		rec.ErrMsg = callErr.Error()
		log.Warn("upstream call failed, re-querying by reqid", "err", callErr)
		rr, rqErr := o.upstream.Requery(ctx, upReq.Reqid)
		if rqErr != nil || rr == nil || !rr.Reachable {
			// 保留上游错误详情(rec.ErrMsg 已含 code/msg/uid/logId)，追加未决说明。
			if rec.ErrMsg == "" {
				rec.ErrMsg = "上游超时/复查未决，PENDING 待对账"
			} else {
				rec.ErrMsg += " | 复查未决，PENDING 待对账"
			}
			log.Warn("re-query unresolved, leaving PENDING for reconciliation", "err", rqErr)
			return queryOutcome{appErr: errs.New(errs.BusiDataRequestErr, "")}
		}
		decision = o.billing.FromRequery(rr)
	} else {
		decision = o.billing.Decide(result)
	}

	// 结算移出关键路径：这里只装配结算工作单，实际 Settle（Redis 计数 + PG 镜像
	// + 台账 UPDATE）由 Bookkeeper 在响应写回后异步执行。
	if decision.Result != nil {
		rec.CalledUpstream = true
		rec.UpstreamCode = decision.Result.Code
		rec.UpstreamUID = decision.Result.UID
		rec.UpstreamLogID = decision.Result.LogID
	}
	rec.Billed = decision.Returned
	return queryOutcome{decision: decision, settleTok: token, settleDec: decision}
}

// respondX1 maps a queryOutcome to the x1 head/body response (DESIGN §6.2/§7.4):
// 查得→body.code 001(累计成功查得数), 查无→body.code 999, 其余→head.errorCode.
func (o *QueryOrchestrator) respondX1(out queryOutcome, requestID string, rec *model.AuditRecord, latencyMs int64) *model.QueryResponse {
	switch {
	case out.existing != nil:
		return o.replay(out.existing, requestID, rec, latencyMs)
	case out.appErr != nil:
		rec.BusiCode = int(out.appErr.Busi)
		rec.BusiMsg = out.appErr.Msg
		return mapping.Error(out.appErr.Busi, out.appErr.Msg, requestID, latencyMs)
	}
	d := out.decision
	switch {
	case d.Resolved && d.Returned && d.Result != nil:
		rec.BusiCode = int(errs.BusiSuccess)
		rec.BusiMsg = "success"
		return mapping.Found(d.Result, requestID, latencyMs)
	case d.Resolved && !d.Returned:
		rec.BusiCode = int(errs.BusiNotFound)
		rec.BusiMsg = "查无结果"
		return mapping.NotFound(d.Result, requestID, latencyMs)
	default:
		rec.BusiCode = int(errs.BusiDataRequestErr)
		rec.ErrMsg = "上游未扣费/我方原因"
		return mapping.Error(errs.BusiDataRequestErr, "", requestID, latencyMs)
	}
}

// replay reconstructs a response from an already-BILLED ledger. The full result
// body is not cached yet, so a查得数据 replay echoes body.code 001 with an empty
// range (TODO: cache the full result keyed by reqid for byte-identical replays).
func (o *QueryOrchestrator) replay(l *model.Ledger, requestID string, rec *model.AuditRecord, latencyMs int64) *model.QueryResponse {
	// 幂等重放也回填台账里的上游标识，保证「上游uid/上游logId」列不因命中缓存而空。
	rec.CalledUpstream = true
	rec.UpstreamUID = l.UpstreamUID
	rec.UpstreamLogID = l.UpstreamLogID
	if l.CountedService {
		rec.BusiCode = int(errs.BusiSuccess)
		rec.BusiMsg = "success"
		return mapping.Found(&model.UpstreamResult{Code: "001", Reqid: l.Reqid, UID: l.UpstreamUID}, requestID, latencyMs)
	}
	rec.BusiCode = int(errs.BusiNotFound)
	rec.BusiMsg = "查无结果"
	return mapping.NotFound(&model.UpstreamResult{Code: "999", Reqid: l.Reqid}, requestID, latencyMs)
}

// QuotaQuery serves the客户配额查询 route (DESIGN §5.2).
func (o *QueryOrchestrator) QuotaQuery(ctx context.Context, signed *model.SignedRequest) (*model.ServiceQuotaView, *model.LicenseView, error) {
	lic, err := o.auth.Authenticate(ctx, signed)
	if err != nil {
		return nil, nil, err
	}
	view, err := o.quota.ServiceQuotaView(ctx, lic, o.route)
	if err != nil {
		return nil, lic, err
	}
	return view, lic, nil
}
