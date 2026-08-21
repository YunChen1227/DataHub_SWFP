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
	// subjects 只在此处**只读**使用（客户自查免费期）；写侧判定在 Bookkeeper 里，
	// 不放在请求关键路径上。
	subjects port.SubjectBillingRepository
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

// WithSubjectBilling 挂接免费期仓储供客户自查（CoverageQuery）。未挂接时该接口
// 返回「本服务未启用主体计费」，查询主流程不受影响。
func (o *QueryOrchestrator) WithSubjectBilling(r port.SubjectBillingRepository) *QueryOrchestrator {
	o.subjects = r
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
	// 结算 + 主体计费 + 审计在响应构造完成后统一提交（异步记账，见 Bookkeeper）。
	// task 在下面各步骤中逐步填齐（闭包捕获变量而非快照，故此处先注册 defer 无妨）：
	// token/decision 由 runCore 在拿到上游确定结论时成对填入，PENDING/失败路径留 nil；
	// sources 是逐源轨迹（含未调用的源），落 upstream_call 子表用于上游成本对账；
	// creditCode/rates 供主体年度计费判定与金额重算。
	task := bookTask{rec: rec}
	defer func() {
		rec.FoundData = rec.BusiCode == int(errs.BusiSuccess)
		rec.LatencyMs = lat()
		rec.CreatedAt = time.Now()
		o.submitBooks(task, log)
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
	task.rates = lic.Rates // 主体计费重算金额时要用（客户合同价优先，缺档走全局缺省）

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
	// 原始请求维度（发票/税务/两者）——与实得维度 DataScope 分开记，二者的差额
	// 就是「客户要了但没给到」的部分，是计费争议时的第一手证据。
	rec.ReqScope = upReq.Want.String()
	// 计费主体落明文：免费期窗口按 (license, creditCode, category) 判定，脱敏值
	// (IDCardMask) 无法作为计费键，也无法用于对客解释免费期。合规依据见
	// 设计_主体年度计费.md §8.1。
	rec.CreditCode = upReq.CreditCode
	task.creditCode = upReq.CreditCode
	log = log.With("reqid", upReq.Reqid)

	// 4-6. Idempotency + reserve + upstream (settle 移交异步记账).
	out := o.runCore(ctx, lic, upReq, requestID, rec, log)
	task.token, task.decision, task.sources = out.settleTok, out.settleDec, out.sources
	return o.respondX1(out, requestID, rec, lat())
}

// submitBooks 提交记账任务：装配了 Bookkeeper 时异步（入队即返回）；否则同步
// 落库（保持旧行为，供测试与未装配场景）。同步路径用独立 ctx——本方法在响应
// 即将写回时执行，请求 ctx 生命周期已不可依赖。
//
// 注意同步降级路径**不做主体年度计费**（免费期仓储挂在 Bookkeeper 上），此时
// decision 保持 Decide 给出的按次计费毛口径。生产环境恒装配 Bookkeeper，该分支
// 只服务于单测与未装配场景。
func (o *QueryOrchestrator) submitBooks(t bookTask, log *slog.Logger) {
	if o.books != nil {
		o.books.Submit(t)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if t.token != nil && t.decision != nil {
		if err := o.quota.Settle(ctx, t.token, t.settleInput()); err != nil {
			log.Error("settle failed", "err", err)
		}
	}
	if o.audit != nil {
		if err := o.audit.AppendAudit(ctx, t.rec); err != nil {
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
	// sources 是逐源寻源轨迹；即便本次失败/PENDING 也要带出——钱已经花了，
	// 明细必须落库。
	sources []model.SourceCall
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
		// 幂等重放本身不产生任何应收——钱在原始那次请求里已经收过（或已判为免费）。
		// 故 Billed 恒为 false，ChargeState 留空表示「本次请求没有自己的计费判定」，
		// 与 NOCHARGE（查无）和 FREE（命中免费期）都不是一回事。
		rec.Billed = false
		rec.FoundData = existing.CountedService
		return queryOutcome{existing: existing}
	}

	result, callErr := o.upstream.Query(ctx, upReq)
	var decision *model.BillingDecision
	var sources []model.SourceCall
	switch {
	case result != nil:
		sources = result.Sources
	case callErr != nil:
		var se *model.UpstreamError
		if errors.As(callErr, &se) {
			sources = se.Sources
		}
	}
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
			rec.UpstreamCostFen = ue.CostFen // 失败单也已产生成本
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
			return queryOutcome{appErr: errs.New(errs.BusiDataRequestErr, ""), sources: sources}
		}
		decision = o.billing.FromRequery(rr)
	} else {
		decision = o.billing.Decide(result, lic.Rates)
	}

	// 结算移出关键路径：这里只装配结算工作单，实际 Settle（Redis 计数 + PG 镜像
	// + 台账 UPDATE）由 Bookkeeper 在响应写回后异步执行。
	if decision.Result != nil {
		rec.CalledUpstream = true
		rec.UpstreamCode = decision.Result.Code
		rec.UpstreamUID = decision.Result.UID
		rec.UpstreamLogID = decision.Result.LogID
		rec.DataScope = decision.Result.Got.String() // 实得维度 = 计费依据
	}
	// 这里落的是**免费期扣减之前**的毛口径：如果没有免费期，这次该按哪档收多少。
	// 记账器随后会执行主体年度计费判定，按真正产生应收的类目重算并覆写这三个字段
	// （见 bookTask.applyChargeToAudit）。未装配主体计费时毛口径即终值。
	rec.FeeStandard = string(decision.Standard)
	rec.AmountFen = decision.AmountFen
	rec.UpstreamCostFen = decision.CostFen
	rec.Billed = decision.Returned
	return queryOutcome{decision: decision, settleTok: token, settleDec: decision, sources: sources}
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

// CoverageQuery 让客户自查某主体的免费期状态：这家企业的发票/税务分别免到哪天。
//
// 这是「主体年度计费」必须配套的接口——客户看到账单上某次查询没扣费，必须能自己
// 查出原因，否则每一次免费命中都会变成一通客服电话。creditCode 走与查询接口同一个
// 归一化函数，保证两侧算出的计费键一致。
func (o *QueryOrchestrator) CoverageQuery(ctx context.Context, signed *model.SignedRequest, rawCreditCode string) (*model.SubjectCoverage, error) {
	lic, err := o.auth.Authenticate(ctx, signed)
	if err != nil {
		return nil, err
	}
	if o.subjects == nil {
		return nil, errs.New(errs.BusiDataRequestErr, "本服务未启用主体计费")
	}
	code, err := parse.NormalizeCreditCode(rawCreditCode)
	if err != nil {
		return nil, err
	}
	cov, err := o.subjects.SubjectCoverage(ctx, lic.LicenseID, o.route, code)
	if err != nil {
		return nil, errs.Wrap(errs.BusiDataRequestErr, "查询失败", err)
	}
	// FreeHits 是内部成本/使用强度指标，不对客暴露。
	cov.Invoice.FreeHits, cov.Tax.FreeHits = 0, 0
	return cov, nil
}
