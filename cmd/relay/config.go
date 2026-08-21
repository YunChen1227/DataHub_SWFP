package main

import (
	"fmt"
	"os"
	"time"

	"github.com/datahub/relay/internal/domain/model"
	"gopkg.in/yaml.v3"
)

// upstreamConfig holds one SWFP upstream sub-source (entcredit or salesdata)。
// 一个条目 = 一次上游调用；多个条目可通过 source 归入同一个「逻辑源」（互补调用，
// 必须一起发出，见 upstream.Source）。
type upstreamConfig struct {
	kind    string // entcredit | salesdata
	baseURL string
	appID     string
	appSecret string
	orgCode         string
	accessKeyID     string
	secretAccessKey string
	product         string
	label           string
	optional        bool

	// 寻源属性（缺省值由 label 推导，见 sourceOf/providesOf，老配置无需改动）。
	source   string // 逻辑源名：寻源优先级列表的单位，也是「已请求过」去重的键
	provides string // invoice | tax | both：该源能提供的维度
	priority int    // 越小越先调用；同优先级按成本从低到高
	costFen  int64  // 该次调用的上游成本（分）
	costOn   string // hit=仅查得计成本（缺省）/ call=调用即计成本
}

type dbConfig struct {
	host     string
	port     int
	name     string
	user     string
	password string
	sslmode  string
	maxConns int
}

type redisConfig struct {
	addr     string
	username string
	password string
	db       int
	poolSize int
}

type versionConfig struct {
	upstreams []upstreamConfig
	db        dbConfig
	redis     redisConfig
}

func (v versionConfig) upstreamKind() string {
	if len(v.upstreams) == 0 {
		return ""
	}
	return v.upstreams[0].kind
}

func (d dbConfig) dsn() string {
	return fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s connect_timeout=10 pool_max_conns=%d",
		d.host, d.port, d.user, d.password, d.name, d.sslmode, d.maxConns,
	)
}

type config struct {
	addr string

	upstreamTimeout time.Duration
	sourcingBudget  time.Duration // 一次请求内全部上游调用的总时延预算（串行寻源）
	defaultRates    model.FeeRates
	freeWindow      string // 主体年度计费的免费期长度（Postgres interval 字面量）
	requeryInterval time.Duration
	demoAppSecret   string
	demoSeed        bool

	adminUser      string
	adminPass      string
	adminJWTSecret string
	adminTokenTTL  time.Duration
	spaDir         string

	storageDriver string
	migrationsDir string

	versions map[string]versionConfig
}

type duration time.Duration

func (d *duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	if s == "" {
		*d = 0
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = duration(parsed)
	return nil
}

type fileUpstream struct {
	Kind            string `yaml:"kind"`
	BaseURL         string `yaml:"baseURL"`
	AppID           string `yaml:"appId"`
	AppSecret       string `yaml:"appSecret"`
	OrgCode         string `yaml:"orgCode"`
	AccessKeyID     string `yaml:"accessKeyId"`
	SecretAccessKey string `yaml:"secretAccessKey"`
	Product         string `yaml:"product"`
	Label           string `yaml:"label"`
	Optional        bool   `yaml:"optional"`
	Source          string `yaml:"source"`
	Provides        string `yaml:"provides"`
	Priority        int    `yaml:"priority"`
	CostFen         int64  `yaml:"costFen"`
	CostOn          string `yaml:"costOn"`
}

type fileDatabase struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Name     string `yaml:"name"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	SSLMode  string `yaml:"sslmode"`
	MaxConns int    `yaml:"maxConns"`
}

type fileRedis struct {
	Addr     string `yaml:"addr"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`
	PoolSize int    `yaml:"poolSize"`
}

type fileVersion struct {
	Upstreams []fileUpstream `yaml:"upstreams"`
	Upstream  fileUpstream   `yaml:"upstream"`
	Database  fileDatabase   `yaml:"database"`
	Redis     fileRedis      `yaml:"redis"`
}

type fileConfig struct {
	Addr     string `yaml:"addr"`
	Upstream struct {
		Timeout duration `yaml:"timeout"`
		// budget 是一次请求内全部上游调用的总时延预算：串行寻源最坏情况是逐源
		// 相加，必须有总闸门，否则命中靠后的源会把下游拖到超时。
		Budget duration `yaml:"budget"`
	} `yaml:"upstream"`
	Billing struct {
		RequeryInterval duration `yaml:"requeryInterval"`
		// rates 是三档收费标准的全局缺省单价（分）；license 上有合同价时优先取
		// 合同价，仅缺失的档位由此兜底。
		Rates struct {
			BothFen    int64 `yaml:"bothFen"`
			InvoiceFen int64 `yaml:"invoiceFen"`
			TaxFen     int64 `yaml:"taxFen"`
		} `yaml:"rates"`
		// freeWindow 是主体年度计费的免费期长度，**Postgres 日历 interval 字面量**
		// （缺省 "1 year"）。周年制：2026-12-28 首次计费则免到 2027-12-28。必须用
		// 日历 interval 而非固定小时数，否则闰年会差一天。
		FreeWindow string `yaml:"freeWindow"`
	} `yaml:"billing"`
	Admin struct {
		BootstrapUser string   `yaml:"bootstrapUser"`
		BootstrapPass string   `yaml:"bootstrapPass"`
		JWTSecret     string   `yaml:"jwtSecret"`
		TokenTTL      duration `yaml:"tokenTTL"`
		SPADir        string   `yaml:"spaDir"`
	} `yaml:"admin"`
	Demo struct {
		AppSecret string `yaml:"appSecret"`
		Seed      *bool  `yaml:"seed"`
	} `yaml:"demo"`
	Storage struct {
		Driver        string `yaml:"driver"`
		MigrationsDir string `yaml:"migrationsDir"`
	} `yaml:"storage"`
	Versions map[string]fileVersion `yaml:"versions"`
}

func loadConfig() (config, error) {
	path := os.Getenv("CONFIG_FILE")
	explicit := path != ""
	if path == "" {
		path = "config.yaml"
	}

	var fc fileConfig
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := yaml.Unmarshal(raw, &fc); err != nil {
			return config{}, fmt.Errorf("parse config %s: %w", path, err)
		}
	case explicit:
		return config{}, fmt.Errorf("read config %s: %w", path, err)
	default:
		fmt.Fprintf(os.Stderr, "warning: %s not found; using non-sensitive defaults, secrets empty\n", path)
	}

	cfg := config{
		addr:            def(fc.Addr, ":8080"),
		upstreamTimeout: durOr(fc.Upstream.Timeout, 4*time.Second),
		sourcingBudget:  durOr(fc.Upstream.Budget, 9*time.Second),
		defaultRates: model.FeeRates{
			BothFen:    fc.Billing.Rates.BothFen,
			InvoiceFen: fc.Billing.Rates.InvoiceFen,
			TaxFen:     fc.Billing.Rates.TaxFen,
		},
		freeWindow:      def(fc.Billing.FreeWindow, model.DefaultFreeWindow),
		requeryInterval: durOr(fc.Billing.RequeryInterval, 10*time.Second),
		demoAppSecret:   def(fc.Demo.AppSecret, "demo-app-secret"),
		demoSeed:        demoSeedOr(fc.Demo.Seed, false),

		adminUser:      def(fc.Admin.BootstrapUser, "admin"),
		adminPass:      fc.Admin.BootstrapPass,
		adminJWTSecret: fc.Admin.JWTSecret,
		adminTokenTTL:  durOr(fc.Admin.TokenTTL, 8*time.Hour),
		spaDir:         def(fc.Admin.SPADir, "web/admin/dist"),

		storageDriver: def(fc.Storage.Driver, "memory"),
		migrationsDir: def(fc.Storage.MigrationsDir, "migrations"),

		versions: make(map[string]versionConfig, len(model.Versions)),
	}

	for _, v := range model.Versions {
		fv, ok := fc.Versions[v]
		if !ok {
			continue
		}

		files := fv.Upstreams
		if len(files) == 0 {
			files = []fileUpstream{fv.Upstream}
		}
		ups := make([]upstreamConfig, 0, len(files))
		for _, fu := range files {
			ups = append(ups, toUpstreamConfig(fu, v))
		}

		cfg.versions[v] = versionConfig{
			upstreams: ups,
			db: dbConfig{
				host:     fv.Database.Host,
				port:     intOr(fv.Database.Port, 5432),
				name:     fv.Database.Name,
				user:     fv.Database.User,
				password: fv.Database.Password,
				sslmode:  def(fv.Database.SSLMode, "disable"),
				maxConns: intOr(fv.Database.MaxConns, 10),
			},
			redis: redisConfig{
				addr:     fv.Redis.Addr,
				username: fv.Redis.Username,
				password: fv.Redis.Password,
				db:       fv.Redis.DB,
				poolSize: intOr(fv.Redis.PoolSize, 10),
			},
		}
	}
	return cfg, nil
}

func toUpstreamConfig(fu fileUpstream, version string) upstreamConfig {
	return upstreamConfig{
		kind:            def(fu.Kind, defaultKind(version)),
		baseURL:         fu.BaseURL,
		appID:           fu.AppID,
		appSecret:       fu.AppSecret,
		orgCode:         fu.OrgCode,
		accessKeyID:     fu.AccessKeyID,
		secretAccessKey: fu.SecretAccessKey,
		product:         fu.Product,
		label:           fu.Label,
		optional:        fu.Optional,
		source:          fu.Source,
		provides:        fu.Provides,
		priority:        fu.Priority,
		costFen:         fu.CostFen,
		costOn:          fu.CostOn,
	}
}

func defaultKind(version string) string {
	return "entcredit"
}

func def(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func demoSeedOr(v *bool, fallback bool) bool {
	if v == nil {
		return fallback
	}
	return *v
}

func intOr(v, fallback int) int {
	if v == 0 {
		return fallback
	}
	return v
}

func durOr(d duration, fallback time.Duration) time.Duration {
	if d == 0 {
		return fallback
	}
	return time.Duration(d)
}
