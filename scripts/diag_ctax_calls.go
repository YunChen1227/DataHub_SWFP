//go:build ignore

// diag_ctax_calls: 只读排查「huizhong 源6 调用量 vs 我方记录」的差异，直接产出多 sheet 的 Excel。
//
//	CONFIG_FILE=config.aliyun.prod.yaml go run ./scripts/diag_ctax_calls.go
//
// 可选环境变量：
//
//	DAYS=30                     统计窗口（天），缺省 30
//	LOG_FILE=logs/relay.log     relay 日志；不存在时跳过日志分析
//	OUT=test_res/xxx.xlsx       输出路径，缺省 test_res/ctax_diag_<时间>.xlsx
//
// 数据库会话强制 default_transaction_read_only=on，不会写库。
//
// 库里无法区分「huizhong 明确查无(SYS404)」与「huizhong 回了 data 但没有业务段」——两者在
// ctax.go 里都归一成 999 落库。后者会打一条 Info 日志（带 huizhong 原始 code），故从
// relay.log 里捞出来、按 reqid 回库关联订单号，两种情况才能拆开。
package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"gopkg.in/yaml.v3"
)

const nodataMsg = "ctax 应答有 data 但无业务数据段"

var cst = time.FixedZone("CST", 8*3600)

type diagConfig struct {
	Versions map[string]struct {
		Database struct {
			Host     string `yaml:"host"`
			Port     int    `yaml:"port"`
			Name     string `yaml:"name"`
			User     string `yaml:"user"`
			Password string `yaml:"password"`
			SSLMode  string `yaml:"sslmode"`
		} `yaml:"database"`
	} `yaml:"versions"`
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func fail(format string, a ...any) {
	fmt.Printf(format+"\n", a...)
	os.Exit(1)
}

func main() {
	configFile := env("CONFIG_FILE", "config.aliyun.prod.yaml")
	logFile := env("LOG_FILE", "logs/relay.log")
	days, _ := strconv.Atoi(env("DAYS", "30"))
	if days <= 0 {
		days = 30
	}
	now := time.Now()
	out := env("OUT", filepath.Join("test_res", "ctax_diag_"+now.In(cst).Format("20060102_1504")+".xlsx"))
	windowStart := now.Add(-time.Duration(days) * 24 * time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	conn, dbDesc := connect(ctx, configFile)
	defer conn.Close(ctx)

	since := fmt.Sprintf("now() - interval '%d days'", days)
	d := &diag{ctx: ctx, conn: conn}

	fmt.Printf("== diag_ctax_calls  %s  最近 %d 天 ==\n", dbDesc, days)

	// ---- 数据库 ----
	total := d.table(`
SELECT (SELECT count(*) FROM audit_log) AS audit_rows,
       (SELECT count(*) FROM billing_ledger) AS ledger_rows,
       (SELECT count(*) FROM upstream_call) AS call_rows,
       (SELECT min(created_at) FROM upstream_call) AS calls_from,
       (SELECT max(created_at) FROM upstream_call) AS calls_to`)

	sum := d.row(`
SELECT count(*) FILTER (WHERE status<>'skipped')                     AS called,
       count(*) FILTER (WHERE status='ok')                           AS ok,
       count(*) FILTER (WHERE status='empty')                        AS empty,
       count(*) FILTER (WHERE status='error')                        AS err,
       count(*) FILTER (WHERE status='skipped')                      AS skipped,
       count(*) FILTER (WHERE status<>'skipped' AND upstream_uid<>'') AS with_order,
       count(*) FILTER (WHERE status='error' AND upstream_uid='')     AS err_no_order,
       count(*) FILTER (WHERE status='error' AND (upstream_msg ILIKE '%deadline%' OR upstream_msg ILIKE '%timeout%')) AS timeouts
FROM upstream_call WHERE source_label='ctax' AND created_at >= ` + since)

	ledgerNoCalls := d.scalar(`
SELECT count(*) FROM billing_ledger l
WHERE l.created_at >= ` + since + `
  AND NOT EXISTS (SELECT 1 FROM upstream_call u WHERE u.app_key=l.app_key AND u.reqid=l.reqid)`)
	callsNoAudit := d.scalar(`
SELECT count(DISTINCT u.request_id) FROM upstream_call u
WHERE u.created_at >= ` + since + `
  AND NOT EXISTS (SELECT 1 FROM audit_log a WHERE a.request_id=u.request_id)`)

	dailyDB := d.table(`
SELECT date(created_at AT TIME ZONE 'Asia/Shanghai')::text AS d,
       count(*) FILTER (WHERE status='ok')    AS ok,
       count(*) FILTER (WHERE status='empty') AS empty,
       count(*) FILTER (WHERE status='error') AS err
FROM upstream_call
WHERE source_label='ctax' AND status<>'skipped' AND created_at >= ` + since + `
GROUP BY 1 ORDER BY 1`)

	reasons := d.table(`
SELECT status,
       CASE WHEN upstream_uid='' THEN '无' ELSE '有' END AS order_no,
       upstream_code,
       CASE WHEN upstream_msg ILIKE '%deadline%' OR upstream_msg ILIKE '%timeout%' THEN '[超时]'
            WHEN upstream_msg LIKE '%频繁%' THEN '[限流]'
            ELSE left(upstream_msg, 120) END AS reason,
       count(*) AS cnt
FROM upstream_call
WHERE source_label='ctax' AND status<>'skipped' AND created_at >= ` + since + `
GROUP BY 1,2,3,4 ORDER BY 5 DESC LIMIT 100`)

	latency := d.table(`
SELECT status, count(*) AS cnt,
       round(percentile_cont(0.5)  WITHIN GROUP (ORDER BY latency_ms)::numeric, 0)::bigint AS p50,
       round(percentile_cont(0.9)  WITHIN GROUP (ORDER BY latency_ms)::numeric, 0)::bigint AS p90,
       round(percentile_cont(0.99) WITHIN GROUP (ORDER BY latency_ms)::numeric, 0)::bigint AS p99,
       max(latency_ms) AS max_ms,
       count(*) FILTER (WHERE latency_ms >= 3900) AS ge_3900ms
FROM upstream_call
WHERE source_label='ctax' AND status<>'skipped' AND created_at >= ` + since + `
GROUP BY 1 ORDER BY 1`)

	skips := d.table(`
SELECT skip_reason, count(*) AS cnt FROM upstream_call
WHERE source_label='ctax' AND status='skipped' AND created_at >= ` + since + `
GROUP BY 1 ORDER BY 2 DESC`)

	compare := d.table(`
WITH a AS (
  SELECT date(created_at AT TIME ZONE 'Asia/Shanghai') AS d, count(*) AS audit_total,
         count(*) FILTER (WHERE called_upstream) AS audit_called,
         count(*) FILTER (WHERE found_data) AS audit_found
  FROM audit_log WHERE created_at >= ` + since + ` GROUP BY 1),
l AS (
  SELECT date(created_at AT TIME ZONE 'Asia/Shanghai') AS d, count(*) AS ledger
  FROM billing_ledger WHERE created_at >= ` + since + ` GROUP BY 1),
c AS (
  SELECT date(created_at AT TIME ZONE 'Asia/Shanghai') AS d, count(*) AS ctax_called
  FROM upstream_call WHERE source_label='ctax' AND status<>'skipped' AND created_at >= ` + since + ` GROUP BY 1),
days AS (SELECT d FROM a UNION SELECT d FROM l UNION SELECT d FROM c)
SELECT days.d::text AS d, COALESCE(a.audit_total,0) AS audit_total, COALESCE(a.audit_called,0) AS audit_called,
       COALESCE(a.audit_found,0) AS audit_found, COALESCE(l.ledger,0) AS ledger, COALESCE(c.ctax_called,0) AS ctax_called
FROM days LEFT JOIN a ON a.d=days.d LEFT JOIN l ON l.d=days.d LEFT JOIN c ON c.d=days.d
ORDER BY 1`)

	byClient := d.table(`
SELECT u.app_key, COALESCE(li.name,'') AS name, count(*) AS ctax_called,
       count(*) FILTER (WHERE u.status='ok') AS ok,
       count(*) FILTER (WHERE u.status='empty') AS empty,
       count(*) FILTER (WHERE u.status='error') AS err
FROM upstream_call u LEFT JOIN license li ON li.app_key=u.app_key
WHERE u.source_label='ctax' AND u.status<>'skipped' AND u.created_at >= ` + since + `
GROUP BY 1,2 ORDER BY 3 DESC`)

	// ---- 日志 ----
	lg := scanLog(logFile, windowStart)
	matched := d.lookupReqids(lg.reqids())

	// ---- 汇总数字 ----
	called, ok, empty, errCnt := toInt(sum["called"]), toInt(sum["ok"]), toInt(sum["empty"]), toInt(sum["err"])
	nodata := int64(len(lg.inWindow))
	explicitEmpty := empty - nodata
	matchedCnt := int64(0)
	for _, m := range matched {
		if m != nil {
			matchedCnt++
		}
	}

	wb := &workbook{}

	// 1. 结论摘要
	coverage := "日志不可用：" + lg.err
	if lg.err == "" {
		coverage = fmt.Sprintf("%s ~ %s", fmtTime(lg.first), fmtTime(lg.last))
		if !lg.first.IsZero() && lg.first.After(windowStart) {
			coverage += fmt.Sprintf("（晚于统计窗口起点 %s，窗口前段的日志缺失，「有 data 无业务段」会偏少）", fmtTime(windowStart))
		}
	}
	summary := [][]any{
		{"统计窗口", fmt.Sprintf("%s ~ %s", fmtTime(windowStart), fmtTime(now)), fmt.Sprintf("最近 %d 天，北京时间", days)},
		{"ctax 实际调用（打到 huizhong）", called, "ok + empty + error；不含 skipped"},
		{"  其中 查得 ok", ok, "我方判为查得并可计费"},
		{"  其中 查无 empty", empty, "库里统一存 999，包含下面两种"},
		{"    · 有 data 但无业务段", nodata, "来自 relay.log；huizhong 回了数据容器（多为只有纳税人基本信息），可能被 huizhong 记为成功"},
		{"    · 明确查无（推算）", explicitEmpty, "= 查无 − 有 data 无业务段；huizhong 返回 SYS404/404 等查无码"},
		{"  其中 失败 error", errCnt, "超时、鉴权/参数错误、限流等"},
		{"    · 超时", toInt(sum["timeouts"]), "我方 4s 断开，无订单号，huizhong 可能仍计一次"},
		{"    · 失败且无订单号", toInt(sum["err_no_order"]), "无法与 huizhong 逐笔对账"},
		{"带订单号的调用", toInt(sum["with_order"]), "可与 huizhong 按 orderNo 逐笔对账"},
		{"ctax 被跳过 skipped", toInt(sum["skipped"]), "priority=1 时应为 0"},
		{"有台账但无逐源明细", ledgerNoCalls, "异步记账丢明细；应为 0"},
		{"有逐源明细但无审计", callsNoAudit, "统计页主表漏请求；应为 0"},
		{"relay.log 覆盖时间", coverage, logFile},
		{"有 data 无业务段：日志条数（窗口内）", nodata, "按 reqid 回库关联"},
		{"有 data 无业务段：库里匹配到", matchedCnt, "匹配不到的多为台账已过期/日志 reqid 非本库"},
	}
	wb.add("结论摘要", []string{"项目", "数值", "说明"}, summary, []float64{34, 26, 90})

	// 2. ctax 按天（库 + 日志合并）
	dailyRows := [][]any{}
	var tOK, tEmpty, tNodata, tErr int64
	seen := map[string]bool{}
	for _, r := range dailyDB.rows {
		day := fmt.Sprint(r[0])
		seen[day] = true
		o, e, er := toInt(r[1]), toInt(r[2]), toInt(r[3])
		nd := lg.byDay[day]
		dailyRows = append(dailyRows, []any{day, o, e, nd, e - nd, er, o + e + er})
		tOK, tEmpty, tNodata, tErr = tOK+o, tEmpty+e, tNodata+nd, tErr+er
	}
	for day, nd := range lg.byDay {
		if !seen[day] {
			dailyRows = append(dailyRows, []any{day, int64(0), int64(0), nd, -nd, int64(0), int64(0)})
			tNodata += nd
		}
	}
	sort.Slice(dailyRows, func(i, j int) bool { return dailyRows[i][0].(string) < dailyRows[j][0].(string) })
	dailyRows = append(dailyRows, []any{"合计", tOK, tEmpty, tNodata, tEmpty - tNodata, tErr, tOK + tEmpty + tErr})
	wb.add("ctax按天", []string{"日期", "查得 ok", "查无 empty（库）", "其中 有data无业务段（日志）", "其中 明确查无（推算）", "失败 error", "合计实调"},
		dailyRows, []float64{14, 10, 16, 24, 20, 12, 12})

	// 3. 有 data 无业务段：按 huizhong 原始码 × 请求维度
	type codeKey struct{ code, want string }
	codeCnt := map[codeKey][2]int64{}
	for _, e := range lg.inWindow {
		k := codeKey{e.Code, e.Want}
		v := codeCnt[k]
		v[0]++
		if matched[e.Reqid] != nil {
			v[1]++
		}
		codeCnt[k] = v
	}
	codeRows := [][]any{}
	for k, v := range codeCnt {
		codeRows = append(codeRows, []any{k.code, k.want, v[0], v[1]})
	}
	sort.Slice(codeRows, func(i, j int) bool { return codeRows[i][2].(int64) > codeRows[j][2].(int64) })
	wb.add("无业务段_按返回码", []string{"huizhong 原始 code", "请求维度 want", "次数", "库里匹配到"}, codeRows, []float64{20, 18, 10, 12})

	// 4. 有 data 无业务段：逐笔明细（带订单号，可直接发 huizhong 核对）
	detail := make([][]any, 0, len(lg.inWindow))
	for _, e := range lg.inWindow {
		row := []any{fmtTime(e.Time), e.Reqid, e.Want, e.Code}
		if m := matched[e.Reqid]; m != nil {
			row = append(row, m.appKey, m.name, m.orderNo, m.status, m.requestID)
		} else {
			row = append(row, "", "", "", "库里未找到", "")
		}
		detail = append(detail, row)
	}
	wb.add("无业务段_明细", []string{"时间", "reqid", "请求维度", "huizhong 原始 code", "app_key", "客户", "huizhong 订单号", "库里状态", "request_id"},
		detail, []float64{20, 22, 14, 16, 16, 30, 24, 12, 38})

	// 5~10. 库表原样
	wb.add("ctax结果分类", reasons.headers, reasons.rows, []float64{10, 8, 14, 70, 10})
	wb.add("ctax耗时", latency.headers, latency.rows, []float64{10, 10, 10, 10, 10, 10, 12})
	wb.add("ctax跳过原因", skips.headers, skips.rows, []float64{50, 10})
	wb.add("审计台账对比", compare.headers, compare.rows, []float64{14, 12, 12, 12, 10, 12})
	wb.add("按客户", byClient.headers, byClient.rows, []float64{16, 32, 12, 8, 10, 8})
	wb.add("全库总量", total.headers, total.rows, []float64{12, 12, 12, 22, 22})

	// 11. 日志覆盖
	wb.add("日志覆盖", []string{"项目", "值"}, [][]any{
		{"日志文件", logFile},
		{"读取错误", lg.err},
		{"文件大小 (MB)", float64(lg.size) / (1 << 20)},
		{"总行数", lg.lines},
		{"带时间戳的 JSON 行", lg.jsonLines},
		{"最早日志时间", fmtTime(lg.first)},
		{"最晚日志时间", fmtTime(lg.last)},
		{"有 data 无业务段（全部日志）", lg.allNodata},
		{"有 data 无业务段（统计窗口内）", nodata},
	}, []float64{30, 60})

	// 12. 口径说明
	wb.add("口径说明", []string{"字段", "含义"}, [][]any{
		{"查得 ok", "huizhong 返回的业务段里至少有一个维度有数据，我方计费"},
		{"查无 empty", "库里 upstream_code=999。包含：① huizhong 返回 SYS404/404/A03002 等查无码；② huizhong 返回了 data 但发票/税务业务段全为空（通常只有纳税人基本信息 nsrjbxx）"},
		{"有data无业务段", "上面的第 ② 种，只能从 relay.log 的「" + nodataMsg + "」日志识别；huizhong 侧可能视为成功调用"},
		{"明确查无（推算）", "查无 − 有data无业务段；若日志覆盖不全会偏大"},
		{"失败 error", "超时 / 鉴权失败(A20029) / 参数缺失(A03001) / 授权校验失败(203) / 限流(500) 等"},
		{"huizhong 订单号", "upstream_call.upstream_uid，即 huizhong 应答里的 orderNo，与对方逐笔对账的键"},
		{"统计页「成功次数」", "audit_log.found_data，仅统计查得；ctax 查无/失败不计入"},
	}, []float64{20, 110})

	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		fail("创建输出目录失败: %v", err)
	}
	if err := wb.save(out); err != nil {
		fail("写 Excel 失败: %v", err)
	}
	fmt.Printf("ctax 实调 %d（查得 %d / 查无 %d / 失败 %d）；有 data 无业务段 %d（库里匹配 %d）\n",
		called, ok, empty, errCnt, nodata, matchedCnt)
	fmt.Println("已输出:", out)
}

// ---------------- 数据库 ----------------

func connect(ctx context.Context, configFile string) (*pgx.Conn, string) {
	raw, err := os.ReadFile(configFile)
	if err != nil {
		fail("读取配置失败: %v", err)
	}
	var cfg diagConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		fail("解析配置失败: %v", err)
	}
	db := cfg.Versions["swfp"].Database
	if db.Host == "" {
		fail("配置里没有 versions.swfp.database")
	}
	if db.Port == 0 {
		db.Port = 5432
	}
	if db.SSLMode == "" {
		db.SSLMode = "disable"
	}
	dsn := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s connect_timeout=15",
		db.Host, db.Port, db.User, db.Password, db.Name, db.SSLMode)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		fail("连接失败: %v", err)
	}
	for _, s := range []string{"SET default_transaction_read_only = on", "SET statement_timeout = '120s'"} {
		if _, err := conn.Exec(ctx, s); err != nil {
			fail("%s 失败: %v", s, err)
		}
	}
	return conn, fmt.Sprintf("db=%s@%s/%s", db.User, db.Host, db.Name)
}

type diag struct {
	ctx  context.Context
	conn *pgx.Conn
}

type result struct {
	headers []string
	rows    [][]any
}

func (d *diag) table(sql string) result {
	rows, err := d.conn.Query(d.ctx, sql)
	if err != nil {
		fail("查询失败: %v\n%s", err, sql)
	}
	defer rows.Close()
	var res result
	fds := rows.FieldDescriptions()
	for _, f := range fds {
		res.headers = append(res.headers, f.Name)
	}
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			fail("读取结果失败: %v", err)
		}
		for i, v := range vals {
			if t, ok := v.(time.Time); ok {
				if fds[i].DataTypeOID == 1082 { // date
					vals[i] = t.Format("2006-01-02")
				} else {
					vals[i] = fmtTime(t)
				}
			}
		}
		res.rows = append(res.rows, vals)
	}
	if err := rows.Err(); err != nil {
		fail("读取结果失败: %v", err)
	}
	return res
}

func (d *diag) row(sql string) map[string]any {
	r := d.table(sql)
	out := map[string]any{}
	if len(r.rows) == 0 {
		return out
	}
	for i, h := range r.headers {
		out[h] = r.rows[0][i]
	}
	return out
}

func (d *diag) scalar(sql string) int64 {
	r := d.table(sql)
	if len(r.rows) == 0 || len(r.rows[0]) == 0 {
		return 0
	}
	return toInt(r.rows[0][0])
}

type callMatch struct {
	appKey, name, orderNo, status, requestID string
}

// lookupReqids 按 reqid 回库找 ctax 那一行，拿到 huizhong 订单号与客户。
func (d *diag) lookupReqids(reqids []string) map[string]*callMatch {
	out := map[string]*callMatch{}
	const batch = 2000
	for i := 0; i < len(reqids); i += batch {
		j := min(i+batch, len(reqids))
		rows, err := d.conn.Query(d.ctx, `
SELECT u.reqid, u.app_key, COALESCE(li.name,''), u.upstream_uid, u.status, u.request_id
FROM upstream_call u LEFT JOIN license li ON li.app_key=u.app_key
WHERE u.source_label='ctax' AND u.reqid = ANY($1)`, reqids[i:j])
		if err != nil {
			fail("按 reqid 回库失败: %v", err)
		}
		for rows.Next() {
			var reqid string
			var m callMatch
			if err := rows.Scan(&reqid, &m.appKey, &m.name, &m.orderNo, &m.status, &m.requestID); err != nil {
				rows.Close()
				fail("读取 reqid 匹配结果失败: %v", err)
			}
			out[reqid] = &m
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			fail("读取 reqid 匹配结果失败: %v", err)
		}
	}
	return out
}

func toInt(v any) int64 {
	switch t := v.(type) {
	case int64:
		return t
	case int32:
		return int64(t)
	case int:
		return int64(t)
	case float64:
		return int64(t)
	}
	return 0
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.In(cst).Format("2006-01-02 15:04:05")
}

// ---------------- 日志 ----------------

type nodataEntry struct {
	Time  time.Time `json:"time"`
	Reqid string    `json:"reqid"`
	Want  string    `json:"want"`
	Code  string    `json:"code"`
}

type logScan struct {
	err         string
	size        int64
	lines       int64
	jsonLines   int64
	first, last time.Time
	allNodata   int64
	inWindow    []nodataEntry
	byDay       map[string]int64
}

func (l *logScan) reqids() []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(l.inWindow))
	for _, e := range l.inWindow {
		if e.Reqid != "" && !seen[e.Reqid] {
			seen[e.Reqid] = true
			out = append(out, e.Reqid)
		}
	}
	return out
}

var timePrefix = []byte(`{"time":"`)

// scanLog 逐行读 relay.log（slog JSON，一行一条）。行长不设上限：ReadBytes 而非 Scanner。
func scanLog(path string, windowStart time.Time) *logScan {
	res := &logScan{byDay: map[string]int64{}}
	f, err := os.Open(path)
	if err != nil {
		res.err = err.Error()
		return res
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil {
		res.size = st.Size()
	}
	needle := []byte(nodataMsg)
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			res.lines++
			if bytes.HasPrefix(line, timePrefix) {
				rest := line[len(timePrefix):]
				if k := bytes.IndexByte(rest, '"'); k > 0 {
					if t, perr := time.Parse(time.RFC3339Nano, string(rest[:k])); perr == nil {
						res.jsonLines++
						if res.first.IsZero() {
							res.first = t
						}
						res.last = t
					}
				}
			}
			if bytes.Contains(line, needle) {
				var e nodataEntry
				if json.Unmarshal(bytes.TrimSpace(line), &e) == nil {
					res.allNodata++
					if !e.Time.Before(windowStart) {
						res.inWindow = append(res.inWindow, e)
						res.byDay[e.Time.In(cst).Format("2006-01-02")]++
					}
				}
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				res.err = err.Error()
			}
			break
		}
	}
	return res
}

// ---------------- 最小 xlsx 写出（仅标准库） ----------------

type sheet struct {
	name    string
	headers []string
	rows    [][]any
	widths  []float64
}

type workbook struct{ sheets []sheet }

func (w *workbook) add(name string, headers []string, rows [][]any, widths []float64) {
	w.sheets = append(w.sheets, sheet{name: name, headers: headers, rows: rows, widths: widths})
}

func (w *workbook) save(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	z := zip.NewWriter(f)
	put := func(name, body string) error {
		fw, err := z.Create(name)
		if err != nil {
			return err
		}
		_, err = io.WriteString(fw, body)
		return err
	}

	var ct, wbx, rels strings.Builder
	ct.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
<Default Extension="xml" ContentType="application/xml"/>
<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>
<Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>
`)
	wbx.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets>`)
	rels.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`)
	for i, s := range w.sheets {
		n := i + 1
		fmt.Fprintf(&ct, `<Override PartName="/xl/worksheets/sheet%d.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`+"\n", n)
		fmt.Fprintf(&wbx, `<sheet name="%s" sheetId="%d" r:id="rId%d"/>`, xmlEsc(s.name), n, n)
		fmt.Fprintf(&rels, `<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet%d.xml"/>`, n, n)
		if err := put(fmt.Sprintf("xl/worksheets/sheet%d.xml", n), sheetXML(s)); err != nil {
			return err
		}
	}
	ct.WriteString(`</Types>`)
	wbx.WriteString(`</sheets></workbook>`)
	fmt.Fprintf(&rels, `<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/></Relationships>`, len(w.sheets)+1)

	for _, p := range [][2]string{
		{"[Content_Types].xml", ct.String()},
		{"_rels/.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/></Relationships>`},
		{"xl/workbook.xml", wbx.String()},
		{"xl/_rels/workbook.xml.rels", rels.String()},
		{"xl/styles.xml", stylesXML},
	} {
		if err := put(p[0], p[1]); err != nil {
			return err
		}
	}
	return z.Close()
}

// 样式 0=普通 1=表头(白字深蓝底) 2=自动换行
const stylesXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
<fonts count="2"><font><sz val="11"/><name val="Calibri"/></font><font><b/><sz val="11"/><color rgb="FFFFFFFF"/><name val="Calibri"/></font></fonts>
<fills count="3"><fill><patternFill patternType="none"/></fill><fill><patternFill patternType="gray125"/></fill><fill><patternFill patternType="solid"><fgColor rgb="FF1F4E78"/><bgColor indexed="64"/></patternFill></fill></fills>
<borders count="1"><border><left/><right/><top/><bottom/><diagonal/></border></borders>
<cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs>
<cellXfs count="3"><xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/><xf numFmtId="0" fontId="1" fillId="2" borderId="0" xfId="0" applyFont="1" applyFill="1"/><xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0" applyAlignment="1"><alignment wrapText="1" vertical="top"/></xf></cellXfs>
<cellStyles count="1"><cellStyle name="Normal" xfId="0" builtinId="0"/></cellStyles>
</styleSheet>`

func sheetXML(s sheet) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetViews><sheetView workbookViewId="0"><pane ySplit="1" topLeftCell="A2" activePane="bottomLeft" state="frozen"/></sheetView></sheetViews>`)
	if len(s.widths) > 0 {
		b.WriteString("<cols>")
		for i, wd := range s.widths {
			fmt.Fprintf(&b, `<col min="%d" max="%d" width="%.1f" customWidth="1"/>`, i+1, i+1, wd)
		}
		b.WriteString("</cols>")
	}
	b.WriteString("<sheetData>")
	header := make([]any, len(s.headers))
	for i, h := range s.headers {
		header[i] = h
	}
	writeRow(&b, 1, header, 1)
	for i, r := range s.rows {
		writeRow(&b, i+2, r, 2)
	}
	if len(s.rows) == 0 {
		writeRow(&b, 2, []any{"(无数据)"}, 2)
	}
	b.WriteString("</sheetData></worksheet>")
	return b.String()
}

func writeRow(b *strings.Builder, rowNum int, vals []any, style int) {
	fmt.Fprintf(b, `<row r="%d">`, rowNum)
	for i, v := range vals {
		ref := colName(i) + strconv.Itoa(rowNum)
		switch t := v.(type) {
		case nil:
			continue
		case int, int32, int64, float32, float64:
			fmt.Fprintf(b, `<c r="%s" s="%d"><v>%v</v></c>`, ref, style, t)
		default:
			fmt.Fprintf(b, `<c r="%s" s="%d" t="inlineStr"><is><t xml:space="preserve">%s</t></is></c>`, ref, style, xmlEsc(fmt.Sprint(t)))
		}
	}
	b.WriteString("</row>")
}

func colName(i int) string {
	s := ""
	for i++; i > 0; i = (i - 1) / 26 {
		s = string(rune('A'+(i-1)%26)) + s
	}
	return s
}

func xmlEsc(s string) string {
	var b bytes.Buffer
	for _, r := range s {
		switch {
		case r == '&':
			b.WriteString("&amp;")
		case r == '<':
			b.WriteString("&lt;")
		case r == '>':
			b.WriteString("&gt;")
		case r == '"':
			b.WriteString("&quot;")
		case r == '\t' || r == '\n' || r == '\r' || r >= 0x20:
			b.WriteRune(r)
		}
	}
	return b.String()
}
