// Package bill24 是「近 24 个月口径」月度对账单脚本的公共实现（scripts/bill_*_24m.go）。
//
// 流程：直接查生产库抓当月查得的 (税号, 接口, 首次请求时间) → 按请求时间重调
// （下游走 swfp 中继，上游直连各数据源）→ 以请求月为锚取 [请求月-24, 请求月] 窗口
// 判定该接口范围内的总金额是否 <= 0 → 按冠辰月度对账单的格式出表，每月一个文件。
//
// 接口维度按客户/我方实际调用的接口分三类：只调用票、只调用税、同时调用票税。
// 判定范围随接口：票看开票金额，税看税务金额，票税看两者之和；<= 0 即不计费。
package bill24

import (
	"fmt"
	"math/big"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Months 是 24 个月口径的回溯月数。
const Months = 24

var cst = time.FixedZone("CST", 8*3600)

// Iface 是三类接口。
type Iface string

const (
	IfaceInvoice Iface = "invoice"
	IfaceTax     Iface = "tax"
	IfaceBoth    Iface = "both"
)

// Ifaces 是对账单里的固定行序。
var Ifaces = []Iface{IfaceInvoice, IfaceTax, IfaceBoth}

func (i Iface) Invoice() bool { return i == IfaceInvoice || i == IfaceBoth }
func (i Iface) Tax() bool     { return i == IfaceTax || i == IfaceBoth }

// Product 是对账单「产品名称」列的固定文案。
func (i Iface) Product() string {
	switch i {
	case IfaceInvoice:
		return "企业经营上下游状况分析模型（发票）"
	case IfaceTax:
		return "纳税评估模型（税务）"
	}
	return "企业经营上下游状况分析模型 (发票)+纳税评估模型 (税务)"
}

// Label 是明细/日志里的短名。
func (i Iface) Label() string {
	switch i {
	case IfaceInvoice:
		return "只调用票"
	case IfaceTax:
		return "只调用税"
	}
	return "同时调用票税"
}

func ifaceOf(inv, tax bool) (Iface, bool) {
	switch {
	case inv && tax:
		return IfaceBoth, true
	case inv:
		return IfaceInvoice, true
	case tax:
		return IfaceTax, true
	}
	return "", false
}

// ifaceOfScope 解析 audit_log.req_scope（invoice / tax / invoice+tax）。
func ifaceOfScope(scope string) (Iface, bool) {
	s := strings.ToLower(scope)
	return ifaceOf(strings.Contains(s, "invoice"), strings.Contains(s, "tax"))
}

// Item 是一条待复核的查得记录：同一个月内同一 (税号, 接口) 只保留首次。
type Item struct {
	Credit    string    `json:"credit"`
	Iface     Iface     `json:"iface"`
	At        time.Time `json:"at"`
	RequestID string    `json:"requestId"`
	AppKey    string    `json:"appKey"`
}

func (it Item) Key() string { return it.Credit + "|" + string(it.Iface) }

// 复核结论。
const (
	OutcomeBill    = "计费"
	OutcomeZero    = "不计费"
	OutcomeMiss    = "查无"
	OutcomeNoData  = "未取得"
	OutcomeFailed  = "失败"
	OutcomeSkipped = "未重调"
)

// Result 是一条记录的复核结果（progress.jsonl 一行）。
type Result struct {
	Item
	Outcome string `json:"outcome"`
	From    int    `json:"from"`
	To      int    `json:"to"`
	Inv     string `json:"inv,omitempty"`
	Tax     string `json:"tax,omitempty"`
	Total   string `json:"total,omitempty"`
	Company string `json:"company,omitempty"`
	Note    string `json:"note,omitempty"`
}

// judge 按接口范围合计金额并给出结论。
func judge(r *Result, inv, tax *big.Rat) {
	total := new(big.Rat)
	if r.Iface.Invoice() {
		r.Inv = inv.FloatString(2)
		total.Add(total, inv)
	}
	if r.Iface.Tax() {
		r.Tax = tax.FloatString(2)
		total.Add(total, tax)
	}
	r.Total = total.FloatString(2)
	if total.Sign() <= 0 {
		r.Outcome = OutcomeZero
	} else {
		r.Outcome = OutcomeBill
	}
}

// ---------------- 配置 ----------------

// DBConfig 是 versions.swfp.database。
type DBConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Name     string `yaml:"name"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	SSLMode  string `yaml:"sslmode"`
}

// UpstreamConfig 是 versions.swfp.upstreams[] 里本工具用到的字段。
type UpstreamConfig struct {
	Kind            string `yaml:"kind"`
	Label           string `yaml:"label"`
	Source          string `yaml:"source"`
	Product         string `yaml:"product"`
	BaseURL         string `yaml:"baseURL"`
	OrgCode         string `yaml:"orgCode"`
	AccessKeyID     string `yaml:"accessKeyId"`
	SecretAccessKey string `yaml:"secretAccessKey"`
	AppID           string `yaml:"appId"`
	AppSecret       string `yaml:"appSecret"`
	Token           string `yaml:"token"`
	AuthCode        string `yaml:"authCode"`
}

// Config 是从 CONFIG_FILE 读出的 swfp 路由配置。
type Config struct {
	DB        DBConfig
	Upstreams []UpstreamConfig
}

func loadConfig() (*Config, error) {
	path := env("CONFIG_FILE", "config.aliyun.prod.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读配置 %s: %w", path, err)
	}
	var f struct {
		Versions map[string]struct {
			Database  DBConfig         `yaml:"database"`
			Upstreams []UpstreamConfig `yaml:"upstreams"`
		} `yaml:"versions"`
	}
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("解析配置 %s: %w", path, err)
	}
	v, ok := f.Versions[env("ROUTE", "swfp")]
	if !ok {
		return nil, fmt.Errorf("配置 %s 里没有 versions.%s", path, env("ROUTE", "swfp"))
	}
	db := v.Database
	// 数据库连接可用环境变量覆盖（换登录账号用，如 DB_USER=chenyun_dev_new）。
	db.Host = env("DB_HOST", db.Host)
	db.Name = env("DB_NAME", db.Name)
	db.User = env("DB_USER", db.User)
	db.Password = env("DB_PASSWORD", db.Password)
	db.SSLMode = env("DB_SSLMODE", db.SSLMode)
	db.Port = envInt("DB_PORT", db.Port)
	return &Config{DB: db, Upstreams: v.Upstreams}, nil
}

// upstreamsOf 返回某个 provider 的全部配置条目（证通有发票/税务两条）。
func (c *Config) upstreamsOf(kind string) []UpstreamConfig {
	var out []UpstreamConfig
	for _, u := range c.Upstreams {
		if u.Kind == kind {
			out = append(out, u)
		}
	}
	return out
}

// ---------------- 运行参数 ----------------

// Month 是一个北京时间自然月 [From, To)。
type Month struct {
	Name     string // 2026-09
	From, To time.Time
}

func parseMonths(s string) ([]Month, error) {
	var out []Month
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		t, err := time.ParseInLocation("2006-01", p, cst)
		if err != nil {
			return nil, fmt.Errorf("MONTHS 里的 %q 不是 yyyy-MM: %w", p, err)
		}
		out = append(out, Month{Name: p, From: t, To: t.AddDate(0, 1, 0)})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("MONTHS 为空")
	}
	return out, nil
}

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if n, err := strconv.Atoi(env(k, "")); err == nil && n > 0 {
		return n
	}
	return def
}
