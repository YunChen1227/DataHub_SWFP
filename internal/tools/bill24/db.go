package bill24

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

func connect(ctx context.Context, c DBConfig) (*pgx.Conn, error) {
	port := c.Port
	if port == 0 {
		port = 5432
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(c.User, c.Password),
		Host:     c.Host + ":" + strconv.Itoa(port),
		Path:     "/" + c.Name,
		RawQuery: "sslmode=" + def(c.SSLMode, "disable") + "&connect_timeout=15",
	}
	conn, err := pgx.Connect(ctx, u.String())
	if err != nil {
		return nil, fmt.Errorf("连接数据库 %s/%s (user=%s): %w", c.Host, c.Name, c.User, err)
	}
	fmt.Printf("已连接数据库 %s/%s  user=%s\n", c.Host, c.Name, c.User)
	return conn, nil
}

// customerAppKeys 返回 license.name 模糊匹配或 app_key 精确匹配 customer 的全部 app_key 与展示名。
func customerAppKeys(ctx context.Context, db *pgx.Conn, customer string) (keys []string, name string, err error) {
	rows, err := db.Query(ctx, `SELECT app_key, name FROM license WHERE name LIKE '%' || $1 || '%' OR app_key = $1 ORDER BY created_at`, customer)
	if err != nil {
		return nil, "", fmt.Errorf("查 license: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k, n string
		if err := rows.Scan(&k, &n); err != nil {
			return nil, "", err
		}
		keys = append(keys, k)
		if name == "" {
			name = n
		}
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if len(keys) == 0 {
		return nil, "", fmt.Errorf("license.name 里没有匹配 %q 的客户", customer)
	}
	return keys, name, nil
}

// creditExpr 取税号：audit_log.credit_code 是迁移 0008 才加的列，早期行回落到 request_payload。
const creditExpr = `COALESCE(NULLIF(a.credit_code, ''), p.credit_code, '')`

// fetchDownstream 抓某客户当月查得的请求：接口取 req_scope（客户调用的接口）。
func fetchDownstream(ctx context.Context, db *pgx.Conn, route string, appKeys []string, m Month) ([]Item, error) {
	rows, err := db.Query(ctx, `
SELECT a.created_at, `+creditExpr+`, a.req_scope, a.request_id, a.app_key
FROM audit_log a
LEFT JOIN request_payload p ON p.request_id = a.request_id
WHERE a.version = $1 AND a.app_key = ANY($2) AND a.found_data
  AND a.created_at >= $3 AND a.created_at < $4
ORDER BY a.created_at`, route, appKeys, m.From, m.To)
	if err != nil {
		return nil, fmt.Errorf("查 audit_log: %w", err)
	}
	defer rows.Close()
	var items []Item
	for rows.Next() {
		var it Item
		var scope string
		if err := rows.Scan(&it.At, &it.Credit, &scope, &it.RequestID, &it.AppKey); err != nil {
			return nil, err
		}
		iface, ok := ifaceOfScope(scope)
		if !ok {
			iface = IfaceBoth // req_scope 为空的早期行：swfp 缺省 dataType 即票税
		}
		it.Iface = iface
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

// fetchUpstream 抓某上游当月 status=ok 的调用（不分下游客户）。同一 request_id 的
// 多条调用（证通发票 + 税务）合并成一个接口；调用维度与请求的 req_scope 取交集——
// 综合源（惠众/税友）的轨迹里 dims 记的是该源能力，不一定是本次要的维度。
func fetchUpstream(ctx context.Context, db *pgx.Conn, route, provider string, m Month) ([]Item, error) {
	rows, err := db.Query(ctx, `
SELECT u.request_id, u.created_at, u.dim_invoice, u.dim_tax, `+creditExpr+`, COALESCE(a.req_scope, ''), u.app_key
FROM upstream_call u
JOIN audit_log a ON a.request_id = u.request_id
LEFT JOIN request_payload p ON p.request_id = u.request_id
WHERE u.version = $1 AND u.provider = $2 AND u.status = 'ok'
  AND u.created_at >= $3 AND u.created_at < $4
ORDER BY u.created_at`, route, provider, m.From, m.To)
	if err != nil {
		return nil, fmt.Errorf("查 upstream_call: %w", err)
	}
	defer rows.Close()

	type agg struct {
		at          time.Time
		credit, key string
		scope       string
		inv, tax    bool
	}
	byReq := map[string]*agg{}
	var order []string
	for rows.Next() {
		var reqID, credit, scope, appKey string
		var at time.Time
		var inv, tax bool
		if err := rows.Scan(&reqID, &at, &inv, &tax, &credit, &scope, &appKey); err != nil {
			return nil, err
		}
		a := byReq[reqID]
		if a == nil {
			a = &agg{at: at, credit: credit, scope: scope, key: appKey}
			byReq[reqID] = a
			order = append(order, reqID)
		}
		a.inv = a.inv || inv
		a.tax = a.tax || tax
		if at.Before(a.at) {
			a.at = at
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var items []Item
	for _, id := range order {
		a := byReq[id]
		inv, tax := a.inv, a.tax
		if want, ok := ifaceOfScope(a.scope); ok {
			inv, tax = inv && want.Invoice(), tax && want.Tax()
		}
		iface, ok := ifaceOf(inv, tax)
		if !ok || a.credit == "" {
			continue
		}
		items = append(items, Item{Credit: a.credit, Iface: iface, At: a.at, RequestID: id, AppKey: a.key})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].At.Before(items[j].At) })
	return items, nil
}

// withCredit 去掉没有税号的记录，保留每次调用。
func withCredit(items []Item) []Item {
	out := items[:0:0]
	for _, it := range items {
		if it.Credit != "" {
			out = append(out, it)
		}
	}
	return out
}

// dedupe 同一 (税号, 接口) 只保留首次请求；结果按请求时间排序。
func dedupe(items []Item) []Item {
	seen := map[string]int{}
	var out []Item
	for _, it := range items {
		if it.Credit == "" {
			continue
		}
		if i, ok := seen[it.Key()]; ok {
			if it.At.Before(out[i].At) {
				out[i] = it
			}
			continue
		}
		seen[it.Key()] = len(out)
		out = append(out, it)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

func def(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
