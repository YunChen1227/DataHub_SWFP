// Package allowlist 实现全局上游白名单闸门：税号 + 上游 provider + 票/税维度。
// 管理后台可热切换；关闭时 Check / Allowed 一律放行（寻源仍受各源自身闸门约束，
// 例如 sy 的 CSV）。开启时：
//   - Check：税号无条目或维度并集不覆盖 Want → 硬拒绝（不发上游）
//   - Allowed(code, provider)：该上游允许的维度；无条目为空集（寻源跳过）
package allowlist

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/datahub/relay/internal/common/errs"
	"github.com/datahub/relay/internal/domain/model"
	"github.com/datahub/relay/internal/domain/parse"
	"github.com/datahub/relay/internal/domain/port"
)

// SettingKeyEnabled 是 system_setting 里的开关键。
const SettingKeyEnabled = "upstream_allowlist_enabled"

// Gate 是进程内可热切换的白名单闸。repo 可为 nil（纯内存，测试用）。
type Gate struct {
	mu      sync.RWMutex
	enabled bool
	// creditCode → provider → DimSet
	byCode map[string]map[string]model.DimSet
	repo   port.UpstreamAllowlistRepository
}

// New 构造空闸（默认关闭）。
func New(repo port.UpstreamAllowlistRepository) *Gate {
	return &Gate{
		byCode: make(map[string]map[string]model.DimSet),
		repo:   repo,
	}
}

// LoadEntries 用 entries 替换内存名单（维度按 (code, provider) 取并集）。
func (g *Gate) LoadEntries(entries []*model.UpstreamAllowEntry) error {
	next := make(map[string]map[string]model.DimSet)
	for i, e := range entries {
		if e == nil {
			continue
		}
		code, err := parse.NormalizeCreditCode(e.CreditCode)
		if err != nil {
			return fmt.Errorf("白名单第 %d 条税号非法: %q", i+1, e.CreditCode)
		}
		provider := e.Provider
		if !validProvider(provider) {
			return fmt.Errorf("白名单第 %d 条 provider 非法: %q", i+1, provider)
		}
		d := e.Dims()
		if d.Empty() {
			return fmt.Errorf("白名单第 %d 条 tax/invoice 全为 0: %s/%s", i+1, code, provider)
		}
		byProv, ok := next[code]
		if !ok {
			byProv = make(map[string]model.DimSet)
			next[code] = byProv
		}
		byProv[provider] = byProv[provider].Union(d)
	}
	g.mu.Lock()
	g.byCode = next
	g.mu.Unlock()
	return nil
}

// LoadFromRepo 从仓储加载开关与名单到内存。repo 为 nil 时无操作。
func (g *Gate) LoadFromRepo(ctx context.Context) error {
	if g.repo == nil {
		return nil
	}
	val, err := g.repo.GetSetting(ctx, SettingKeyEnabled)
	if err != nil {
		return err
	}
	entries, err := g.repo.ListAllowlist(ctx)
	if err != nil {
		return err
	}
	if err := g.LoadEntries(entries); err != nil {
		return err
	}
	g.mu.Lock()
	g.enabled = val == "true" || val == "1"
	g.mu.Unlock()
	return nil
}

// Enabled 返回开关是否打开。
func (g *Gate) Enabled() bool {
	if g == nil {
		return false
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.enabled
}

// SetEnabled 热切换开关；有 repo 时先落库再改内存。
func (g *Gate) SetEnabled(ctx context.Context, enabled bool) error {
	if g == nil {
		return fmt.Errorf("上游白名单闸未装配")
	}
	if g.repo != nil {
		v := "false"
		if enabled {
			v = "true"
		}
		if err := g.repo.SetSetting(ctx, SettingKeyEnabled, v); err != nil {
			return err
		}
	}
	g.mu.Lock()
	g.enabled = enabled
	g.mu.Unlock()
	return nil
}

// Allowed 返回 creditCode 在指定 provider 上允许的维度。
// 闸未开或 g 为 nil 时返回 AllDims（表示本闸不限制）；开启且无条目时返回空集。
func (g *Gate) Allowed(creditCode, provider string, now time.Time) model.DimSet {
	_ = now
	if g == nil || !g.Enabled() {
		return model.AllDims()
	}
	code, err := parse.NormalizeCreditCode(creditCode)
	if err != nil {
		return model.DimSet{}
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	byProv := g.byCode[code]
	if byProv == nil {
		return model.DimSet{}
	}
	return byProv[provider]
}

// Union 返回该税号在所有上游上的允许维度并集。闸未开时返回 AllDims。
func (g *Gate) Union(creditCode string) model.DimSet {
	if g == nil || !g.Enabled() {
		return model.AllDims()
	}
	code, err := parse.NormalizeCreditCode(creditCode)
	if err != nil {
		return model.DimSet{}
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	var u model.DimSet
	for _, d := range g.byCode[code] {
		u = u.Union(d)
	}
	return u
}

// Check 在闸开启时校验税号+维度：无条目或不覆盖 want → 1009。闸关闭恒成功。
func (g *Gate) Check(creditCode string, want model.DimSet) error {
	if g == nil || !g.Enabled() {
		return nil
	}
	if want.Empty() {
		want = model.AllDims()
	}
	u := g.Union(creditCode)
	if u.Empty() {
		return errs.New(errs.BusiServiceNotOpen, "服务尚未开通：该税号不在上游白名单")
	}
	if !u.Covers(want) {
		return errs.New(errs.BusiServiceNotOpen,
			fmt.Sprintf("服务尚未开通：该税号白名单仅允许 %s，本次请求 %s", u.String(), want.String()))
	}
	return nil
}

// Stats 返回内存名单计数。
func (g *Gate) Stats() model.UpstreamAllowStats {
	st := model.UpstreamAllowStats{ByProvider: map[string]int{}}
	if g == nil {
		return st
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	st.UniqueCodes = len(g.byCode)
	for _, byProv := range g.byCode {
		for provider, d := range byProv {
			st.Total++
			st.ByProvider[provider]++
			switch {
			case d.Both():
				st.Both++
			case d.Tax:
				st.TaxOnly++
			default:
				st.InvoiceOnly++
			}
		}
	}
	return st
}

// View 返回管理接口视图。
func (g *Gate) View() model.UpstreamAllowlistView {
	return model.UpstreamAllowlistView{Enabled: g.Enabled(), Stats: g.Stats()}
}

func validProvider(p string) bool {
	switch p {
	case "entcredit", "ctax", "sy", "salesdata":
		return true
	}
	return false
}
