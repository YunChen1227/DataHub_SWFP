package admin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/datahub/relay/internal/common/xlsx"
	"github.com/datahub/relay/internal/domain/port"
	"github.com/datahub/relay/internal/domain/report"
)

// ErrReportsUnavailable：未装配报表仓储时的导出请求。
var ErrReportsUnavailable = errors.New("出账对账导出不可用")

// WithReports 挂接出账/对账导出。
func (s *Service) WithReports(r port.ReportRepository) *Service {
	s.reports = report.New(s.route, r)
	return s
}

// Report 是一次导出的结果：工作簿与建议文件名。
type Report struct {
	Workbook *xlsx.Workbook
	Filename string
}

// DownstreamReport 导出某客户的下游账单。customer 为客户名称（精确匹配，同名的
// 多个 appKey 一起统计）；空串表示全部客户。from/to 为北京时间日期（含 to 当天）。
func (s *Service) DownstreamReport(ctx context.Context, customer, from, to string) (*Report, error) {
	if s.reports == nil {
		return nil, ErrReportsUnavailable
	}
	f, t, err := report.ParseRange(from, to)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	customer = strings.TrimSpace(customer)
	var appKeys []string
	if customer != "" {
		users, err := s.users.ListUsers(ctx, s.route)
		if err != nil {
			return nil, err
		}
		for _, u := range users {
			if u.Name == customer {
				appKeys = append(appKeys, u.AppKey)
			}
		}
		if len(appKeys) == 0 {
			return nil, fmt.Errorf("%w: 找不到客户 %q", ErrValidation, customer)
		}
	}
	wb, err := s.reports.Downstream(ctx, customer, appKeys, f, t)
	if err != nil {
		return nil, err
	}
	name := customer
	if name == "" {
		name = "全部客户"
	}
	return &Report{Workbook: wb, Filename: fmt.Sprintf("下游账单-%s-%s至%s.xlsx", name, from, to)}, nil
}

// UpstreamReport 导出某上游公司的对账单。vendor 为 report.Vendors 里的 ID。
func (s *Service) UpstreamReport(ctx context.Context, vendor, from, to string) (*Report, error) {
	if s.reports == nil {
		return nil, ErrReportsUnavailable
	}
	v, ok := report.VendorByID(strings.TrimSpace(vendor))
	if !ok {
		return nil, fmt.Errorf("%w: 未知上游 %q", ErrValidation, vendor)
	}
	f, t, err := report.ParseRange(from, to)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	wb, err := s.reports.Upstream(ctx, v, f, t)
	if err != nil {
		return nil, err
	}
	return &Report{Workbook: wb, Filename: fmt.Sprintf("上游对账-%s-%s至%s.xlsx", v.Name, from, to)}, nil
}
