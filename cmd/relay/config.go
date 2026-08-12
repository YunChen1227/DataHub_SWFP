package main

import (
	"fmt"
	"os"
	"time"

	"github.com/datahub/relay/internal/domain/model"
	"gopkg.in/yaml.v3"
)

// upstreamConfig holds one SWFP upstream sub-source (entcredit or salesdata).
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
	} `yaml:"upstream"`
	Billing struct {
		RequeryInterval duration `yaml:"requeryInterval"`
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
