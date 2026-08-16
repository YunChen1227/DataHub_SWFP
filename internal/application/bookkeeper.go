package application

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/datahub/relay/internal/domain/model"
	"github.com/datahub/relay/internal/domain/port"
	"github.com/datahub/relay/internal/domain/quota"
)

// bookTask 是一次请求响应后的记账工作单：台账结算（可选）+ 逐源明细 + 审计落库。
// 三者对构造下游响应毫无贡献，从关键路径剥离（DESIGN 异步记账）。
type bookTask struct {
	token    *quota.ReserveToken    // 与 decision 成对；nil = 无需结算（鉴权/参数失败、幂等重放、PENDING）
	decision *model.BillingDecision // 上游确定结论；PENDING 场景为 nil（台账留待复查/对账结算）
	sources  []model.SourceCall     // 逐源寻源轨迹（含 skipped）；无上游调用的路径为空
	rec      *model.AuditRecord     // 恒非 nil：每次请求都写审计
}

// settleInput 组装结算输入。busiCode 取审计记录里的最终下游业务码——它在响应
// 映射完成后才确定，正好由本任务（响应写回后执行）带上。
func (t bookTask) settleInput() quota.SettleInput {
	in := quota.SettleInput{Decision: t.decision, Sources: t.sources}
	if t.rec != nil {
		in.BusiCode = t.rec.BusiCode
	}
	return in
}

// callRecords 把轨迹转成逐源明细行。关联键与台账同构 (appKey, version, reqid)，
// 另带 requestId 供后台按请求下钻。
func (t bookTask) callRecords() []*model.UpstreamCallRecord {
	if len(t.sources) == 0 || t.rec == nil {
		return nil
	}
	out := make([]*model.UpstreamCallRecord, 0, len(t.sources))
	now := time.Now()
	for _, c := range t.sources {
		out = append(out, &model.UpstreamCallRecord{
			SourceCall: c,
			AppKey:     t.rec.AppKey,
			Version:    t.rec.Version,
			Reqid:      t.rec.Reqid,
			RequestID:  t.rec.RequestID,
			Billable:   c.Status == model.CallOK,
			CreatedAt:  now,
		})
	}
	return out
}

// Bookkeeper 把结算 + 审计移出请求关键路径：Handle 构造完响应即入队返回，
// 常驻 worker 用独立 context（请求 ctx 响应后即取消，不能复用）落库。
//
// 可靠性口径：
//   - 背压：队列满或已关闭时降级为「同步执行」——宁可让该请求慢几毫秒，绝不
//     静默丢弃计费台账/审计记录。
//   - 停机：Close() 停止接收并 drain 全部余量后返回（主流程在 HTTP Shutdown
//     后调用，复用现有 10s 优雅停机窗口）。
//   - 进程崩溃窗口：队列中未落库的审计/计数丢失，但 PENDING 台账已在响应前
//     同步写入（崩溃安全锚点），由复查/对账兜底终态化——与 DESIGN §7.3 一致。
//   - 与 RequeryWorker 的边界：本 worker 是同步路径结算的唯一执行者；
//     RequeryWorker 只处理「复查可达」的 PENDING 台账（当前各上游 Requery 均为
//     stub 不可达）。若未来实现真实 Requery，需为其加台账年龄下限，避免与
//     在队列中的毫秒级新 PENDING 抢结算。
type Bookkeeper struct {
	quota *quota.Service
	audit port.AuditRepository
	calls port.UpstreamCallRepository // 逐源明细；nil 时跳过（未装配/测试）
	log   *slog.Logger

	mu     sync.RWMutex // 保护 closed 与 tasks 的发送/关闭竞态
	closed bool
	tasks  chan bookTask
	wg     sync.WaitGroup
}

// NewBookkeeper 启动 workers 个常驻记账协程（queueSize/workers ≤0 时取缺省
// 1024/2）。quota 或 audit 可为 nil（对应操作跳过）。
func NewBookkeeper(q *quota.Service, audit port.AuditRepository, queueSize, workers int, log *slog.Logger) *Bookkeeper {
	if queueSize <= 0 {
		queueSize = 1024
	}
	if workers <= 0 {
		workers = 2
	}
	if log == nil {
		log = slog.Default()
	}
	b := &Bookkeeper{quota: q, audit: audit, log: log, tasks: make(chan bookTask, queueSize)}
	for i := 0; i < workers; i++ {
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			for t := range b.tasks {
				b.process(t)
			}
		}()
	}
	return b
}

// WithUpstreamCalls 挂接逐源明细仓储（upstream_call 表）。未挂接时不落明细，
// 台账里的汇总列仍然可用。
func (b *Bookkeeper) WithUpstreamCalls(r port.UpstreamCallRepository) *Bookkeeper {
	b.calls = r
	return b
}

// Submit 入队一个记账任务；队列满或已关闭时同步执行（背压降级，不丢任务）。
func (b *Bookkeeper) Submit(t bookTask) {
	b.mu.RLock()
	if !b.closed {
		select {
		case b.tasks <- t:
			b.mu.RUnlock()
			return
		default:
			// 队列满：降级同步。
		}
	}
	b.mu.RUnlock()
	b.process(t)
}

// Close 停止接收新任务并 drain 队列（阻塞至全部落库）。幂等。
func (b *Bookkeeper) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	close(b.tasks)
	b.mu.Unlock()
	b.wg.Wait()
}

// process 执行一个任务。必须用独立 context——请求 ctx 在响应写回后即被取消，
// 复用它会导致所有异步落库统一报 context canceled（异步化的经典坑）。
func (b *Bookkeeper) process(t bookTask) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if b.quota != nil && t.token != nil && t.decision != nil {
		if err := b.quota.Settle(ctx, t.token, t.settleInput()); err != nil {
			b.log.Error("async settle failed", "reqid", t.token.Reqid, "err", err)
		}
	}
	// 逐源明细与结算解耦：PENDING/失败路径没有结算，但上游成本照样已经发生，
	// 明细必须落库，否则对账时看不到这笔支出。
	if b.calls != nil {
		if rows := t.callRecords(); len(rows) > 0 {
			if err := b.calls.AppendUpstreamCalls(ctx, rows); err != nil {
				b.log.Error("async upstream calls append failed", "requestId", t.rec.RequestID, "err", err)
			}
		}
	}
	if b.audit != nil && t.rec != nil {
		if err := b.audit.AppendAudit(ctx, t.rec); err != nil {
			b.log.Error("async audit append failed", "requestId", t.rec.RequestID, "err", err)
		}
	}
}
