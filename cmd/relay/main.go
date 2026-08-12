// Command relay is the DataHub_SWFP entrypoint: SWFP tax/invoice aggregation relay.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/datahub/relay/internal/api"
	"github.com/datahub/relay/internal/application"
	"github.com/datahub/relay/internal/domain/admin"
	"github.com/datahub/relay/internal/domain/auth"
	"github.com/datahub/relay/internal/domain/billing"
	"github.com/datahub/relay/internal/domain/model"
	"github.com/datahub/relay/internal/domain/parse"
	"github.com/datahub/relay/internal/domain/port"
	"github.com/datahub/relay/internal/domain/quota"
	"github.com/datahub/relay/internal/infrastructure/persistence/memory"
	"github.com/datahub/relay/internal/infrastructure/persistence/postgres"
	redisq "github.com/datahub/relay/internal/infrastructure/persistence/redis"
	"github.com/datahub/relay/internal/infrastructure/secret"
	"github.com/datahub/relay/internal/infrastructure/upstream"
	"github.com/datahub/relay/internal/job"
)

// domainStorage is one license 域的存储后端 (独立 DB+Redis；v8/v9 共用 v8v9 域)。
// 同一域内的多条路由 (如 v8/v9) 复用这一套 repos，共享 license 表，但统计/台账/
// 审计按各自 route 独立 (见 model.RouteDomain)。
type domainStorage struct {
	licenseRepo port.LicenseRepository
	ledgerRepo  port.LedgerRepository
	quotaRepo   port.QuotaRepository
	auditRepo   port.AuditRepository
	adminRepo   port.AdminUserRepository
	userRepo    port.UserAdminRepository
	secrets     port.SecretProvider
	// auth 是本域共享的鉴权服务（license+secret 进程内缓存）。按域而非按路由建：
	// v8/v9 共用 v8v9 域的同一实例，后台在任一路由改 license 都能命中同一份缓存
	// 失效（admin.WithLicenseChangeHook → auth.Invalidate）。
	auth    *auth.Service
	cleanup func()
}

// routeStack is one fully-wired route (独立 orchestrator + 后台服务 + 复查 worker
// + 异步记账器)，接到其所属域的存储 + 自己的上游客户端。
type routeStack struct {
	orch    *application.QueryOrchestrator
	admin   *admin.Service
	requery *job.RequeryWorker
	books   *application.Bookkeeper
}

// domainOwner returns the route whose db/redis config seeds a domain's storage
// (域内第一个出现的路由)。v8v9 域 → v9 (model.Versions 中 v9 先于 v8)。
func domainOwner(domain string) string {
	for _, r := range model.Versions {
		if model.RouteDomain(r) == domain {
			return r
		}
	}
	return domain
}

// checkStorageIsolation fails fast when两个不同的域被配置成共用同一个 PostgreSQL
// 库或同一个 Redis 逻辑库——那会破坏「各域独立 license/记录」的隔离承诺。
// (v8/v9 同属 v8v9 域，共用其 owner v9 的库属于设计内共享，不在校验之列。)
func checkStorageIsolation(cfg config) error {
	dbSeen := make(map[string]string)    // host:port/name -> domain
	redisSeen := make(map[string]string) // addr/db -> domain
	for _, domain := range model.Domains {
		vc, ok := cfg.versions[domainOwner(domain)]
		if !ok {
			continue
		}
		if vc.db.name != "" {
			key := fmt.Sprintf("%s:%d/%s", vc.db.host, vc.db.port, vc.db.name)
			if prev, dup := dbSeen[key]; dup {
				return fmt.Errorf("域 %s 与 %s 配置了同一个数据库 %s；每个域必须使用独立数据库", prev, domain, key)
			}
			dbSeen[key] = domain
		}
		if vc.redis.addr != "" {
			key := fmt.Sprintf("%s/%d", vc.redis.addr, vc.redis.db)
			if prev, dup := redisSeen[key]; dup {
				return fmt.Errorf("域 %s 与 %s 配置了同一个 Redis 逻辑库 %s；每个域必须使用独立 Redis db", prev, domain, key)
			}
			redisSeen[key] = domain
		}
	}
	return nil
}

func main() {
	level := slog.LevelInfo
	if lv := os.Getenv("LOG_LEVEL"); strings.EqualFold(lv, "debug") {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	cfg, err := loadConfig()
	if err != nil {
		slog.Error("load config failed", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 上游共享 HTTP client：显式 Transport 提高连接复用率。Go 默认
	// MaxIdleConnsPerHost=2——swfp 一次请求就并发 5 路打同一主机，默认值会导致
	// 反复新建 TCP+TLS 连接（每次 50-200ms 握手），是端到端延迟的大头之一。
	httpClient := &http.Client{
		Timeout: cfg.upstreamTimeout,
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			MaxIdleConns:        256,
			MaxIdleConnsPerHost: 64,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 5 * time.Second,
			ForceAttemptHTTP2:   true,
		},
	}

	// --- 存储隔离防呆校验后，按域开库 (v8/v9 共用 v8v9 域库)，再逐路由装配 ---
	if err := checkStorageIsolation(cfg); err != nil {
		logger.Error("storage isolation check failed", "err", err)
		os.Exit(1)
	}

	domainStores := make(map[string]*domainStorage, len(model.Domains))
	cleanups := make([]func(), 0, len(model.Domains))
	defer func() {
		for _, c := range cleanups {
			c()
		}
	}()
	for _, domain := range model.Domains {
		ds, err := buildDomainStorage(ctx, cfg, domain, logger)
		if err != nil {
			logger.Error("build domain storage failed", "domain", domain, "err", err)
			os.Exit(1)
		}
		domainStores[domain] = ds
		if ds.cleanup != nil {
			cleanups = append(cleanups, ds.cleanup)
		}
		logger.Info("domain storage ready", "domain", domain, "driver", cfg.storageDriver,
			"owner", domainOwner(domain))
	}

	apiStacks := make(map[string]*api.VersionStack, len(model.Versions))
	adminByRoute := make(map[string]*admin.Service, len(model.Versions))
	bookkeepers := make([]*application.Bookkeeper, 0, len(model.Versions))
	for _, route := range model.Versions {
		ds := domainStores[model.RouteDomain(route)]
		st, err := buildRouteStack(cfg, route, ds, httpClient, logger)
		if err != nil {
			logger.Error("build route stack failed", "route", route, "err", err)
			os.Exit(1)
		}
		apiStacks[route] = &api.VersionStack{Orch: st.orch, Admin: st.admin}
		adminByRoute[route] = st.admin
		bookkeepers = append(bookkeepers, st.books)
		go st.requery.Run(ctx)
		logger.Info("route stack ready", "route", route, "domain", model.RouteDomain(route),
			"upstream", cfg.versions[route].upstreamKind(), "sources", len(cfg.versions[route].upstreams))
	}

	// 控制面：后台统一登录 + JWT 校验走 swfp 路由的 admin 服务 (swfp 域)。
	control := adminByRoute["swfp"]
	if control == nil {
		logger.Error("swfp stack not built; cannot start admin control plane")
		os.Exit(1)
	}
	if err := control.BootstrapAdmin(ctx, cfg.adminUser, cfg.adminPass); err != nil {
		logger.Error("bootstrap admin failed", "err", err)
	} else {
		logger.Info("admin console ready", "loginUser", cfg.adminUser, "spaDir", cfg.spaDir)
	}

	// --- HTTP server ---
	server := api.NewServer(apiStacks, control, cfg.spaDir)
	httpServer := &http.Server{
		Addr:              cfg.addr,
		Handler:           server.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		logger.Info("relay listening", "addr", cfg.addr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http server failed", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
	// HTTP 已停止接收新请求后再 drain 异步记账队列：保证在途请求的结算/审计
	// 全部落库（宁可多等几百毫秒，不丢计费凭证），随后 defer 里的 cleanup 才关库。
	for _, b := range bookkeepers {
		b.Close()
	}
	logger.Info("bookkeepers drained")
}

// buildDomainStorage opens the storage backend for one license 域 (DB+Redis or
// memory)，使用该域 owner 路由的 db/redis 配置。同一域只建一次，供域内各路由复用。
// 生产 (postgres) 不播种 demo license；memory (开发) 按域播种各自独立的 demo 凭证
// (model.DemoAppKey；v8/v9 同域共用一个)。
func buildDomainStorage(ctx context.Context, cfg config, domain string, logger *slog.Logger) (*domainStorage, error) {
	owner := domainOwner(domain)
	vc := cfg.versions[owner]

	switch cfg.storageDriver {
	case "postgres":
		if vc.db.name == "" {
			return nil, fmt.Errorf("domain %s (owner %s): database.name 未配置", domain, owner)
		}
		pg, err := postgres.New(ctx, vc.db.dsn())
		if err != nil {
			return nil, fmt.Errorf("postgres connect: %w", err)
		}
		if err := postgres.ApplyMigrations(ctx, pg.Pool(), cfg.migrationsDir); err != nil {
			pg.Close()
			return nil, fmt.Errorf("apply migrations: %w", err)
		}
		if cfg.demoSeed {
			if err := postgres.SeedDemo(ctx, pg, owner); err != nil {
				pg.Close()
				return nil, fmt.Errorf("seed demo: %w", err)
			}
		}
		rq, err := redisq.New(ctx, redisq.Options{
			Addr:     vc.redis.addr,
			Username: vc.redis.username,
			Password: vc.redis.password,
			DB:       vc.redis.db,
			PoolSize: vc.redis.poolSize,
		}, pg)
		if err != nil {
			pg.Close()
			return nil, fmt.Errorf("redis connect: %w", err)
		}
		ds := &domainStorage{
			licenseRepo: pg, ledgerRepo: pg, quotaRepo: rq, auditRepo: pg,
			adminRepo: pg, userRepo: pg, secrets: secret.NewStore(pg),
			cleanup: func() { rq.Close(); pg.Close() },
		}
		ds.auth = auth.New(ds.licenseRepo, ds.secrets, auth.Md5Verifier{})
		return ds, nil
	default:
		store := memory.New()
		seedDemo(store, domain, cfg.demoAppSecret)
		ds := &domainStorage{
			licenseRepo: store, ledgerRepo: store, quotaRepo: store, auditRepo: store,
			adminRepo: store, userRepo: store, secrets: secret.NewStore(store),
			cleanup: func() {},
		}
		ds.auth = auth.New(ds.licenseRepo, ds.secrets, auth.Md5Verifier{})
		return ds, nil
	}
}

// buildRouteStack wires the per-route dependencies (auth/quota/billing/orchestrator/
// admin/requery) on top of the route's 域存储 + 自己的上游客户端。
func buildRouteStack(cfg config, route string, ds *domainStorage, httpClient *http.Client, logger *slog.Logger) (*routeStack, error) {
	vc := cfg.versions[route]
	log := logger.With("route", route)

	upClient, routeKind, err := buildUpstreams(route, vc.upstreams, httpClient, log)
	if err != nil {
		return nil, err
	}

	authSvc := ds.auth // 域级共享（含 license 缓存；v8/v9 同域同缓存）
	quotaSvc := quota.New(ds.quotaRepo, ds.ledgerRepo)
	billSvc := billing.New(billing.DefaultTable())
	adminSvc := admin.New(route, ds.adminRepo, ds.userRepo, ds.auditRepo, admin.Config{
		JWTSecret: cfg.adminJWTSecret,
		TokenTTL:  cfg.adminTokenTTL,
	}).WithLicenseChangeHook(authSvc.Invalidate) // 后台改密/停用/删除即时失效鉴权缓存
	// 异步记账：结算 + 审计移出响应关键路径（每请求省 3-5 次串行 DB 写）；
	// 队列满降级同步，优雅停机时 drain（见 main 的 shutdown 顺序）。
	books := application.NewBookkeeper(quotaSvc, ds.auditRepo, 0, 0, log)
	orch := application.NewQueryOrchestrator(route, authSvc, quotaSvc, billSvc, upClient, ds.auditRepo, log).
		WithBookkeeper(books)
	// 网关校验口径必须与该路由上游的真实要求一致（必填字段前置拦截，不透传给
	// 上游报错）。默认 parse.Parse (mobile必/idCard必/name选) 仅适用于与经济能力
	// 同口径的上游 (gama/income)。聚合路由所有子源 kind 一致 (loadConfig 已校验)，
	// 故按路由 kind 选校验器即可。
	switch routeKind {
	case upstream.ProviderEntCredit:
		// swfp 入参对齐上游证通 entcreditapi 的 args.creditCode。
		orch.WithParser(parse.ParseCreditCode)
	}
	requery := job.NewRequeryWorker(ds.ledgerRepo, ds.licenseRepo, upClient, billSvc, quotaSvc, cfg.requeryInterval, log)

	return &routeStack{orch: orch, admin: adminSvc, requery: requery, books: books}, nil
}

// buildUpstreams 把一条路由的上游子源列表装配成一个 port.UpstreamPort：逐条构建
// 单源 client，套上 Aggregator (len==1 直通 / len>1 并发聚合)。返回聚合器与路由 kind
// (=首个子源 kind，loadConfig 已校验同路由 kind 一致；供 parser 选择)。
func buildUpstreams(route string, ucs []upstreamConfig, httpClient *http.Client, logger *slog.Logger) (port.UpstreamPort, string, error) {
	if len(ucs) == 0 {
		// 路由未在配置中给出 (memory 模式常见)：合成一个按路由缺省 kind 的空 client，
		// 保持"不崩溃"的历史行为——该 client 在被调用前不产生任何副作用。
		ucs = []upstreamConfig{{kind: defaultKind(route)}}
	}
	sources := make([]upstream.LabeledUpstream, 0, len(ucs))
	for i, uc := range ucs {
		client, err := buildClient(route, uc, httpClient, logger)
		if err != nil {
			return nil, "", err
		}
		sources = append(sources, upstream.LabeledUpstream{Label: labelFor(uc, i), Port: client, Optional: uc.optional})
	}
	agg, err := upstream.NewAggregator(sources)
	if err != nil {
		return nil, "", err
	}
	// swfp 有明确的下游返回值文档 (docs/税票分析接口文档.xlsx)：套契约映射层，把
	// 聚合分段结果整理为 xlsx 两段结构 + 按源分组 + sourceStatus（严格白名单）。
	// 其余路由无返回值文档，保持原样透传聚合结果。
	if route == "swfp" {
		return upstream.NewSwfpContract(agg), ucs[0].kind, nil
	}
	return agg, ucs[0].kind, nil
}

// labelFor 决定子源在聚合 range 里的段名：显式 label 优先；entcredit 未指定时按
// 产品码缺省 (invoice1/tax1…)；其余回退为 kind+下标 (单源路由用不到 label)。
func labelFor(uc upstreamConfig, idx int) string {
	if uc.label != "" {
		return uc.label
	}
	if uc.kind == upstream.ProviderEntCredit && uc.product != "" {
		return upstream.EntCreditLabel(uc.product)
	}
	if uc.kind == upstream.ProviderSalesData {
		return "sales" // swfp 契约层按此段名映射为源5
	}
	return fmt.Sprintf("%s%d", uc.kind, idx+1)
}

// buildClient constructs one 上游子源 client (port.UpstreamPort) by kind.
func buildClient(version string, uc upstreamConfig, httpClient *http.Client, logger *slog.Logger) (port.UpstreamPort, error) {
	switch uc.kind {
	case upstream.ProviderEntCredit:
		client := upstream.NewEntCredit(upstream.EntCreditConfig{
			Endpoint:        uc.baseURL,
			OrgCode:         uc.orgCode,
			AccessKeyID:     uc.accessKeyID,
			SecretAccessKey: uc.secretAccessKey,
			Product:         uc.product,
		}, httpClient)
		return client, nil
	case upstream.ProviderSalesData:
		// swfp 源5 销项数据 (凯盈云 crestv)：appId=AppID、appSecret=AppKey (兼作
		// AES 密钥，复用既有凭证字段)。baseURL 为业务接口前缀 (…/api/ws)。
		client := upstream.NewSalesData(upstream.SalesDataConfig{
			BaseURL: uc.baseURL,
			AppID:   uc.appID,
			AppKey:  uc.appSecret,
		}, httpClient)
		return client, nil
	default:
		return nil, fmt.Errorf("version %s: unknown upstream kind %q", version, uc.kind)
	}
}

// seedDemo registers the 域's dev demo license in a memory store so the
// e2e/admin flows have a known client per 域。demo appKey 按域各不相同
// (model.DemoAppKey)，保证 demo 凭证无法跨域使用；v8/v9 同域共用一个。
func seedDemo(store *memory.Store, domain, demoSecret string) {
	up := strings.ToUpper(domain)
	store.SeedLicense(&model.LicenseView{
		LicenseID:  "LIC-DEMO-" + up,
		AppKey:     model.DemoAppKey(domainOwner(domain)),
		ClientUUID: "demo-client-" + domain,
		Status:     "ACTIVE",
	}, demoSecret, "Demo 商户("+up+")", "13800001234")
}
