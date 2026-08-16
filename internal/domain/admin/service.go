// Package admin implements the admin console business logic (DESIGN §16):
// operator login (JWT), 普通用户 (license) CRUD, MD5 凭证生成与轮换, 审计查询。
// v0.6 起无额度配置; v0.7 起新增手机号/密钥时间字段并移除 IP 白名单 (IP 准入交由
// 阿里云 ECS 安全组)。
package admin

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/datahub/relay/internal/common/jwt"
	"github.com/datahub/relay/internal/domain/model"
	"github.com/datahub/relay/internal/domain/port"
)

var (
	ErrInvalidCredentials = errors.New("用户名或密码错误")
	ErrUserNotFound       = errors.New("用户不存在")
	ErrValidation         = errors.New("参数校验失败")
	// ErrUpstreamCallsUnavailable：未装配逐源明细仓储（或未给 requestId）时的下钻请求。
	ErrUpstreamCallsUnavailable = errors.New("逐源明细不可用")
)

// Config holds admin session knobs.
type Config struct {
	JWTSecret string
	TokenTTL  time.Duration
}

// Service coordinates the admin repositories. route 标记本后台服务的路由作用域
// (x1/v9/v8/zlf/blk)：用户列表/CRUD 作用于该路由所属域的共享 license，但统计
// (成功查得数/调用次数) 与操作日志按 route 独立 (共享 license 的 v8/v9 互不影响)。
type Service struct {
	route  string
	admins port.AdminUserRepository
	users  port.UserAdminRepository
	audits port.AuditRepository
	// calls 是逐源明细仓储（upstream_call），供审计按源下钻；可为 nil。
	calls port.UpstreamCallRepository
	cfg   Config
	// onLicenseChange 在用户被修改/删除/轮换密钥后回调（参数为 appKey），用于
	// 即时失效鉴权层的 license 缓存（auth.Service.Invalidate），保证停用/换钥
	// 不必等缓存 TTL 过期。可为 nil。
	onLicenseChange func(appKey string)
}

func New(route string, admins port.AdminUserRepository, users port.UserAdminRepository, audits port.AuditRepository, cfg Config) *Service {
	if cfg.TokenTTL <= 0 {
		cfg.TokenTTL = 8 * time.Hour
	}
	return &Service{route: route, admins: admins, users: users, audits: audits, cfg: cfg}
}

// WithLicenseChangeHook 注册 license 变更回调（装配层接 auth 缓存失效）。
func (s *Service) WithLicenseChangeHook(fn func(appKey string)) *Service {
	s.onLicenseChange = fn
	return s
}

// notifyLicenseChange 触发变更回调（nil 安全）。
func (s *Service) notifyLicenseChange(appKey string) {
	if s.onLicenseChange != nil {
		s.onLicenseChange(appKey)
	}
}

// --- §16.1 auth ---

// BootstrapAdmin creates the initial admin from config if it does not exist.
func (s *Service) BootstrapAdmin(ctx context.Context, username, password string) error {
	if username == "" || password == "" {
		return nil
	}
	existing, err := s.admins.FindAdmin(ctx, username)
	if err != nil {
		return err
	}
	if existing != nil {
		return nil
	}
	return s.admins.PutAdmin(ctx, &model.AdminUser{
		Username:     username,
		PasswordHash: HashPassword(password),
		Role:         "ADMIN",
		CreatedAt:    time.Now(),
	})
}

// Login verifies credentials and returns a signed JWT + expiry (unix seconds).
func (s *Service) Login(ctx context.Context, username, password string) (string, int64, error) {
	a, err := s.admins.FindAdmin(ctx, username)
	if err != nil {
		return "", 0, err
	}
	if a == nil || !VerifyPassword(password, a.PasswordHash) {
		return "", 0, ErrInvalidCredentials
	}
	return jwt.Sign(s.cfg.JWTSecret, a.Username, s.cfg.TokenTTL)
}

// VerifyToken validates a bearer token and returns the subject (username).
func (s *Service) VerifyToken(token string) (string, error) {
	c, err := jwt.Verify(s.cfg.JWTSecret, token)
	if err != nil {
		return "", err
	}
	return c.Sub, nil
}

// --- §16.2 user management ---

// CreateUserInput is the new-user payload from the admin UI.
type CreateUserInput struct {
	Name   string
	Mobile string
}

// CreateUserResult returns the created user plus the one-time plaintext secret.
type CreateUserResult struct {
	User   *model.UserDetail `json:"user"`
	Secret string            `json:"secret"` // 仅本次返回, 之后不可读
}

func (s *Service) ListUsers(ctx context.Context) ([]*model.UserDetail, error) {
	return s.users.ListUsers(ctx, s.route)
}

// SearchUsers filters users by uuid(appKey)/名称/手机号 substring.
func (s *Service) SearchUsers(ctx context.Context, keyword string) ([]*model.UserDetail, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return s.users.ListUsers(ctx, s.route)
	}
	return s.users.SearchUsers(ctx, keyword, s.route)
}

func (s *Service) GetUser(ctx context.Context, licenseID string) (*model.UserDetail, error) {
	u, err := s.users.GetUser(ctx, licenseID, s.route)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, ErrUserNotFound
	}
	return u, nil
}

func (s *Service) CreateUser(ctx context.Context, in CreateUserInput) (*CreateUserResult, error) {
	secret := GenerateSecret()
	now := time.Now()
	detail := &model.UserDetail{
		LicenseID:       "LIC-" + strings.ToUpper(randAlpha(10)),
		AppKey:          GenerateAppKey(),
		Name:            strings.TrimSpace(in.Name),
		Mobile:          strings.TrimSpace(in.Mobile),
		Status:          "ACTIVE",
		ClientUUID:      randAlpha(24),
		SecretCreatedAt: now,
		ValidTo:         now.AddDate(10, 0, 0), // 与存储层 3650 天授权期一致
		CreatedAt:       now,
	}
	if err := s.users.CreateUser(ctx, detail, secret); err != nil {
		return nil, err
	}
	return &CreateUserResult{User: detail, Secret: secret}, nil
}

// UpdateUserInput carries the editable fields (empty value = leave unchanged)。
// Rates 为 nil 时不改合同价；非 nil 时整份覆盖（某档填 0 = 该档走全局缺省费率）。
type UpdateUserInput struct {
	Status string
	Mobile string
	Rates  *model.FeeRates
}

func (s *Service) UpdateUser(ctx context.Context, licenseID string, in UpdateUserInput) (*model.UserDetail, error) {
	cur, err := s.users.GetUser(ctx, licenseID, s.route)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, ErrUserNotFound
	}
	status := in.Status
	if status == "" {
		status = cur.Status
	}
	mobile := strings.TrimSpace(in.Mobile)
	if mobile == "" {
		mobile = cur.Mobile
	}
	if err := s.users.UpdateUser(ctx, licenseID, status, mobile); err != nil {
		return nil, err
	}
	if in.Rates != nil {
		if err := s.users.SetRates(ctx, licenseID, *in.Rates); err != nil {
			return nil, err
		}
	}
	s.notifyLicenseChange(cur.AppKey)
	return s.users.GetUser(ctx, licenseID, s.route)
}

func (s *Service) DeleteUser(ctx context.Context, licenseID string) error {
	cur, err := s.users.GetUser(ctx, licenseID, s.route)
	if err != nil {
		return err
	}
	if cur == nil {
		return ErrUserNotFound
	}
	if err := s.users.DeleteUser(ctx, licenseID); err != nil {
		return err
	}
	s.notifyLicenseChange(cur.AppKey)
	return nil
}

// RotateSecret regenerates the user's secret and returns the new plaintext once.
func (s *Service) RotateSecret(ctx context.Context, licenseID string) (string, error) {
	cur, err := s.users.GetUser(ctx, licenseID, s.route)
	if err != nil {
		return "", err
	}
	if cur == nil {
		return "", ErrUserNotFound
	}
	secret := GenerateSecret()
	if err := s.users.RotateSecret(ctx, licenseID, secret); err != nil {
		return "", err
	}
	s.notifyLicenseChange(cur.AppKey)
	return secret, nil
}

// --- §16.3 audits ---

func (s *Service) ListAudits(ctx context.Context, f model.AuditFilter) ([]*model.AuditRecord, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	// 按本后台服务的路由作用域过滤：共享 license 的 v8/v9 操作日志互不混淆。
	f.Version = s.route
	return s.audits.ListAudits(ctx, f)
}

// WithUpstreamCalls 挂接逐源明细仓储，开启审计的「按源下钻」。
func (s *Service) WithUpstreamCalls(r port.UpstreamCallRepository) *Service {
	s.calls = r
	return s
}

// ListUpstreamCalls 返回某次请求的逐源调用明细（成本对账的原子记录）：调了哪些源、
// 各自的上游订单号/请求号、耗时与成本，以及未调用的源和跳过的原因。
func (s *Service) ListUpstreamCalls(ctx context.Context, requestID string) ([]*model.UpstreamCallRecord, error) {
	if s.calls == nil {
		return nil, ErrUpstreamCallsUnavailable
	}
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return nil, ErrUpstreamCallsUnavailable
	}
	return s.calls.ListUpstreamCalls(ctx, model.UpstreamCallFilter{
		Version:   s.route,
		RequestID: requestID,
		Limit:     200,
	})
}
