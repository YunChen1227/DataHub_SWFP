//go:build ignore

// reconcile_shuidi: 用我方对水滴（证通 entcredit）的调用记录，核对水滴给的「存疑税号.xlsx」（只读）。
//
//	CONFIG_FILE=config.aliyun.prod.yaml go run ./scripts/reconcile_shuidi.go
//
// 可选环境变量：
//
//	MATCH_SECONDS=60       水滴时间与我方调用时间相差超过多少秒时在备注里标出
//	OUT=test_res/xxx.xlsx  缺省 test_res/水滴存疑税号对账.xlsx
//
// 输出三个 sheet：总览（票/税各自的查得、查无、失败、我方无调用条数）、票明细、税明细。
//
// 口径：
//   - 水滴调用 = upstream_call.provider='entcredit' 且 status<>'skipped'（skipped 表示没发出请求）。
//   - 「票」= source_label invoice1/invoice2（发票数据聚合），「税」= tax1/tax2（税务数据聚合）。
//   - 税号取 audit_log.credit_code（按 request_id 关联），时间按北京时间。
//   - 每条水滴记录在我方找同维度、同税号、时间最近的一次调用；税号的查找不限月份，
//     这样跨月调用也能识别出来。
package main

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"gopkg.in/yaml.v3"
)

var cst = time.FixedZone("CST", 8*3600)

type exportConfig struct {
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

type billRow struct {
	dim, code string
	at        time.Time
}

type call struct {
	dim, code                      string
	at                             time.Time
	label, status                  string
	upCode, upMsg, upUID, customer string
	appKey, requestID              string
	latencyMs                      int64
}

func main() {
	configFile := env("CONFIG_FILE", "config.aliyun.prod.yaml")
	matchSec, err := strconv.ParseFloat(env("MATCH_SECONDS", "60"), 64)
	if err != nil {
		fail("MATCH_SECONDS 应为数字: %v", err)
	}
	out := env("OUT", filepath.Join("test_res", "水滴存疑税号对账.xlsx"))

	bill := parseBill()
	codes := map[string]bool{}
	for _, b := range bill {
		codes[b.code] = true
	}
	codeList := make([]string, 0, len(codes))
	for c := range codes {
		codeList = append(codeList, c)
	}

	raw, err := os.ReadFile(configFile)
	if err != nil {
		fail("读取配置失败: %v", err)
	}
	var cfg exportConfig
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

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	conn, err := pgx.Connect(ctx, fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s connect_timeout=15",
		db.Host, db.Port, db.User, db.Password, db.Name, db.SSLMode))
	if err != nil {
		fail("连接失败: %v", err)
	}
	defer conn.Close(ctx)
	for _, s := range []string{"SET default_transaction_read_only = on", "SET statement_timeout = '180s'"} {
		if _, err := conn.Exec(ctx, s); err != nil {
			fail("%s 失败: %v", s, err)
		}
	}

	byCode := loadCalls(ctx, conn, `a.credit_code = ANY($1)`, codeList)

	group := map[string][]call{}
	for _, c := range byCode {
		k := c.dim + "|" + c.code
		group[k] = append(group[k], c)
	}

	results := []string{"查得", "查无", "失败", "我方无调用"}
	count := map[string]map[string]int{"票": {}, "税": {}}
	mismatch := map[string]int{}
	details := map[string][][]any{"票": {}, "税": {}}
	for _, b := range bill {
		cands := group[b.dim+"|"+b.code]
		var best *call
		bestDiff := math.MaxFloat64
		for i := range cands {
			if d := math.Abs(cands[i].at.Sub(b.at).Seconds()); d < bestDiff {
				best, bestDiff = &cands[i], d
			}
		}
		var row []any
		result, note := "", ""
		if best == nil {
			result = "我方无调用"
			note = "我方没有这个税号该维度的任何调用记录"
			row = []any{b.code, b.at.Format("2006-01-02 15:04:05"), result, nil, nil, nil, nil, nil, nil, nil, nil, int64(0), note}
		} else {
			result = statusName(best.status)
			if bestDiff > matchSec {
				note = fmt.Sprintf("时间相差超过 %g 秒，可能不是同一次调用", matchSec)
				mismatch[b.dim]++
			}
			row = []any{b.code, b.at.Format("2006-01-02 15:04:05"), result,
				best.at.Format("2006-01-02 15:04:05"), int64(math.Round(bestDiff)),
				best.upCode, best.upMsg, best.upUID, best.customer, best.appKey, best.requestID,
				int64(len(cands)), note}
		}
		count[b.dim][result]++
		details[b.dim] = append(details[b.dim], row)
	}

	overview := [][]any{{"维度", "存疑条数", "查得", "查无", "失败", "我方无调用", "其中时间对不上"}}
	totals := make([]int64, 6)
	for _, dim := range []string{"票", "税"} {
		row := []any{dim, int64(len(details[dim]))}
		totals[0] += int64(len(details[dim]))
		for i, r := range results {
			row = append(row, int64(count[dim][r]))
			totals[i+1] += int64(count[dim][r])
		}
		row = append(row, int64(mismatch[dim]))
		totals[5] += int64(mismatch[dim])
		overview = append(overview, row)
	}
	overview = append(overview, []any{"合计", totals[0], totals[1], totals[2], totals[3], totals[4], totals[5]})
	notes := [][]any{
		{},
		{"口径", "水滴 = upstream_call.provider='entcredit'，排除 skipped（未发出请求）"},
		{"", "票 = 发票数据聚合（invoice1/invoice2），税 = 税务数据聚合（tax1/tax2）"},
		{"", "每条存疑记录取我方同维度、同税号、时间最近的一次调用，按该次调用结果判定查得/查无/失败"},
		{"", "查得 = 证通返回 001；查无 = 证通返回 999；失败 = 超时、网关错误或证通 E 开头错误码"},
		{"", fmt.Sprintf("时间相差超过 %g 秒的仍按最近一次调用判定，并单独计入「其中时间对不上」", matchSec)},
		{"", "生成时间 " + time.Now().In(cst).Format("2006-01-02 15:04:05")},
	}
	overview = append(overview, notes...)

	wb := &workbook{}
	wb.add("总览", nil, overview, []float64{10, 12, 10, 10, 10, 12, 16})
	wb.sheets[0].hdrRows = map[int]bool{0: true}
	headers := []string{"税号", "水滴时间", "我方结果", "我方调用时间", "时间差(秒)", "证通返回码", "证通返回信息",
		"水滴订单号", "客户", "appKey", "request id", "我方该税号调用次数", "备注"}
	widths := []float64{22, 20, 12, 20, 10, 10, 20, 26, 28, 16, 40, 12, 34}
	wb.add("票明细", headers, details["票"], widths)
	wb.add("税明细", headers, details["税"], widths)

	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		fail("创建输出目录失败: %v", err)
	}
	if err := wb.save(out); err != nil {
		fail("写 Excel 失败: %v", err)
	}
	for _, r := range overview[:4] {
		fmt.Println(r...)
	}
	fmt.Println("已输出:", out)
}

func loadCalls(ctx context.Context, conn *pgx.Conn, where string, args ...any) []call {
	q := `
SELECT CASE WHEN uc.source_label LIKE 'invoice%' THEN '票' ELSE '税' END,
       COALESCE(a.credit_code, ''), uc.created_at, uc.source_label, uc.status,
       uc.upstream_code, uc.upstream_msg, uc.upstream_uid, COALESCE(l.name, ''),
       uc.app_key, uc.request_id, uc.latency_ms
FROM upstream_call uc
LEFT JOIN LATERAL (
    SELECT credit_code FROM audit_log WHERE request_id = uc.request_id LIMIT 1
) a ON TRUE
LEFT JOIN license l ON l.app_key = uc.app_key
WHERE uc.provider = 'entcredit' AND uc.status <> 'skipped' AND ` + where + `
ORDER BY uc.created_at`
	rows, err := conn.Query(ctx, q, args...)
	if err != nil {
		fail("查询上游调用失败: %v", err)
	}
	defer rows.Close()
	var out []call
	for rows.Next() {
		var c call
		if err := rows.Scan(&c.dim, &c.code, &c.at, &c.label, &c.status,
			&c.upCode, &c.upMsg, &c.upUID, &c.customer, &c.appKey, &c.requestID, &c.latencyMs); err != nil {
			fail("读取上游调用失败: %v", err)
		}
		c.at = c.at.In(cst)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		fail("读取上游调用失败: %v", err)
	}
	return out
}

func parseBill() []billRow {
	var out []billRow
	for _, line := range strings.Split(strings.TrimSpace(billData), "\n") {
		f := strings.Split(strings.TrimSpace(line), ",")
		if len(f) != 3 {
			fail("内嵌账单行格式错误: %q", line)
		}
		t, err := time.ParseInLocation("2006-01-02 15:04:05", f[2], cst)
		if err != nil {
			fail("内嵌账单时间错误: %q", line)
		}
		out = append(out, billRow{dim: f[0], code: f[1], at: t})
	}
	return out
}

func statusName(s string) string {
	switch s {
	case "ok":
		return "查得"
	case "empty":
		return "查无"
	case "error":
		return "失败"
	}
	return s
}

// 存疑税号.xlsx：维度,税号,水滴记录时间（北京时间）
const billData = `
票,91320211MA20CDY24B,2026-09-01 19:39:08
票,91341300MA2WBALG1E,2026-09-02 17:27:20
票,92340223MA2TX8C64D,2026-09-03 19:32:19
票,92340705MA8NGEEB7G,2026-09-03 19:49:50
票,92340705MA8PXBCF3E,2026-09-03 19:49:52
票,91340124MA2MR97N5C,2026-09-09 17:54:17
票,91350102MA8UEBC28K,2026-09-15 13:42:35
票,91370181560799690D,2026-09-20 10:15:06
票,91371000064387689L,2026-09-20 10:15:06
票,91371082MA3M7GKB1R,2026-09-20 10:15:07
票,9137108209171522XP,2026-09-20 10:15:07
票,91371082MA3CBK112R,2026-09-20 10:15:07
票,91371082MA3C0GH645,2026-09-20 10:15:07
票,91371082MA3PU9LJ27,2026-09-20 10:15:08
票,91371082675506587X,2026-09-20 10:15:09
票,9137108272757382XT,2026-09-20 10:15:09
票,91371082MA3CFH298T,2026-09-20 10:15:10
票,91370900MABNXMFY1J,2026-09-20 10:15:10
票,91370112MAC598TU4U,2026-09-20 10:15:11
票,91371312MACRYJD113,2026-09-20 10:15:13
票,91370830MA94E33T88,2026-09-20 10:15:13
票,913406005606911377,2026-09-20 10:15:14
票,91371300MA3WFYDC77,2026-09-20 10:15:15
票,91370705MA3MFFAJ45,2026-09-20 10:15:16
票,91370112MA7KKFA604,2026-09-20 10:15:17
票,91370800562533001C,2026-09-20 10:15:17
票,91340104MA2UD4A97M,2026-09-20 10:15:18
票,91370117MA3WCX8AXC,2026-09-20 10:15:19
票,91370704MAEHG4JD3X,2026-09-20 10:15:20
票,91370700MA94P2RL27,2026-09-20 10:15:20
票,91371312MAC7345Y22,2026-09-20 10:15:21
票,91370702760042190X,2026-09-20 10:15:21
票,91370800MA3P54RG6K,2026-09-20 10:15:21
票,91370702334399365U,2026-09-20 10:15:22
票,9137081173066129X5,2026-09-20 10:15:23
票,91370786076956869P,2026-09-20 10:15:24
票,91370704MA952XMD5F,2026-09-20 10:15:24
票,91370785MA3WJJWT8T,2026-09-20 10:15:25
票,91370828MA3FF4YH46,2026-09-20 10:15:25
票,91370811310485920H,2026-09-20 10:15:26
票,913708110673719817,2026-09-20 10:15:26
票,91370827595213450T,2026-09-20 10:15:26
票,913708006705369205,2026-09-20 10:15:27
票,91370800MA3MH89W6R,2026-09-20 10:15:28
票,91370786MA3EJF7H3F,2026-09-20 10:15:28
票,91370785310435384C,2026-09-20 10:15:29
票,91370782739264856L,2026-09-20 10:15:29
票,913707823218196073,2026-09-20 10:15:29
票,91370782MA3PKQ558D,2026-09-20 10:15:30
票,91370681X13385616B,2026-09-20 10:15:31
票,913706817266900123,2026-09-20 10:15:32
票,913706830699912364,2026-09-20 10:15:32
票,91370611074403718A,2026-09-20 10:15:34
票,91370681MA3CB52L1Y,2026-09-20 10:15:34
票,91370681MA3R7L870F,2026-09-20 10:15:34
票,91370117MABQ3L7M9L,2026-09-24 17:23:24
票,91370104MA953QQF31,2026-09-24 19:02:18
票,91320506552466335P,2026-09-24 20:31:48
票,91370104MA3RMMU161,2026-09-25 05:00:05
票,92341226MA8PBBGX6H,2026-09-25 08:57:37
票,91341002MA8QGRP99C,2026-09-25 12:49:09
票,91371302077974874J,2026-09-25 18:56:10
票,91341700MAE9BYE53M,2026-09-26 08:53:28
票,91341500MA2T2AQ6X1,2026-09-26 09:41:35
票,92340825MA2NQCBH5X,2026-09-26 12:23:40
票,91341800MAG0RWUF45,2026-09-26 13:58:25
票,92370909MA7GDKC49U,2026-09-26 17:57:03
票,913710000673706713,2026-09-27 09:36:51
票,91370102MA3QLTDG3L,2026-09-27 13:26:21
票,91340826MADM4KDQ22,2026-09-27 15:19:51
票,91140621MAK4WKXW8N,2026-09-27 16:01:34
票,91371521MADBXHM58Q,2026-09-27 17:51:25
票,91370104MAEMKKYG34,2026-09-27 23:13:48
票,91371325MAD6G1719J,2026-09-28 09:04:37
票,91370700MA3N4XMD1F,2026-09-28 09:29:23
票,91341000MA2N9TM345,2026-09-28 13:34:15
票,92370181MA3WCXEX6D,2026-09-28 15:06:29
票,91370705MA3WDLG012,2026-09-28 15:33:15
票,91371403MAC54Q7J6L,2026-09-29 10:59:44
票,91420117MA7MHWAP8C,2026-09-29 12:20:34
票,91371103MA3QC5W684,2026-09-29 16:08:55
票,91371102MAET9FGJ62,2026-09-29 18:04:55
票,92341100MAE6118E1X,2026-09-29 20:47:29
票,92340102MA8NPQT20R,2026-09-29 22:01:22
票,92371312MA3FFN6E33,2026-09-30 11:15:38
票,91320206081507136J,2026-09-30 15:06:24
票,911407813966984915,2026-09-30 16:19:58
票,91530100MADCYWLAXH,2026-09-30 16:46:18
票,92340705MA2NMNF933,2026-09-30 17:33:01
票,91371200056238675R,2026-09-30 19:16:07
票,91370600MAK9KL219F,2026-09-30 21:10:22
票,92340207MA2P7CHR30,2026-09-30 22:41:40
税,91320211MA20CDY24B,2026-09-01 19:39:20
税,91350102MA8UEBC28K,2026-09-15 14:01:28
税,91371082MA3M7GKB1R,2026-09-20 10:30:00
税,91371312MAC7345Y22,2026-09-20 10:30:06
税,91110106596038797J,2026-09-24 19:22:12
税,91330502329994493U,2026-09-24 19:22:18
税,91110115MA020RX616,2026-09-24 19:22:54
税,91330600MA288R398W,2026-09-24 19:23:29
税,91150403MA0RR9R56T,2026-09-24 19:23:29
税,91411627MACXF34T1Q,2026-09-24 19:24:32
税,91110105MA01Q81K1H,2026-09-24 19:26:14
税,91421000316572291M,2026-09-24 19:26:32
税,91410100MA9FAQ6R13,2026-09-24 19:26:42
税,91120116MABXN9K989,2026-09-24 19:27:21
税,91330102MAE85QPM7N,2026-09-24 19:28:15
税,91530127MAEX0W4568,2026-09-24 19:29:01
税,9134012256639204XD,2026-09-24 19:30:22
税,91441303MADWUU4N9F,2026-09-24 19:30:49
税,91371403MAC1666313,2026-09-24 19:31:00
税,91320114MABRD7J37C,2026-09-24 19:31:04
税,91340100781084760C,2026-09-24 19:31:09
税,91410803MA40K91JX9,2026-09-24 19:31:55
税,91330109MADXGQT623,2026-09-24 19:31:58
税,91610131MAB0W3WN1G,2026-09-24 19:32:26
税,91430406053867468E,2026-09-25 19:21:15
税,91120110MA069LNR69,2026-09-25 19:21:15
税,91430103MA7BPP4P49,2026-09-25 19:22:07
税,92340300MA2QYUR54W,2026-09-25 19:22:38
税,91341500MAE2Q2B28Y,2026-09-25 19:22:44
税,91360122MA7E5CRK8U,2026-09-25 19:23:24
税,91110108MA00GTW74L,2026-09-25 19:23:37
税,91410902MADH9JJR91,2026-09-25 19:24:09
税,91222500MA173D9W6Y,2026-09-25 19:24:45
税,91320115MA1UWLHX66,2026-09-25 19:25:13
税,91110111MAE9FETD10,2026-09-25 19:25:26
税,91421100MA48XJWU2C,2026-09-25 19:25:29
税,91411025MA9KCJ7X30,2026-09-25 19:25:55
税,91371312MA3T6UF173,2026-09-25 19:26:43
税,92410825MA9GYPNAXK,2026-09-25 19:27:06
税,91350205MA31UC2K6G,2026-09-25 19:28:19
税,91320102598036103G,2026-09-25 19:28:23
税,91320982060167945X,2026-09-25 19:28:35
税,91230110MAC1DGAX8R,2026-09-25 19:29:08
税,913205080566874620,2026-09-25 19:29:32
税,91110105094568819N,2026-09-25 19:30:18
税,91110106091892074F,2026-09-25 19:30:35
税,91110108MA7N96QT9E,2026-09-25 19:30:51
税,91341600MA8P831P7A,2026-09-25 19:30:58
税,91410105MA464P659K,2026-09-26 19:21:42
税,91410181MA9F96EJ7C,2026-09-26 19:22:35
税,91340321MAD9BXWJ81,2026-09-26 19:23:12
税,91330103MA2GM3HYXY,2026-09-26 19:23:43
税,91410721MA9KWGBD6T,2026-09-26 19:25:44
税,91360121MAE08KJ360,2026-09-26 19:25:46
税,91410182MADBDFY17J,2026-09-26 19:25:52
税,91410307MAD65C0G7X,2026-09-26 19:25:55
税,91410523MAEQR7037B,2026-09-26 19:26:46
税,91210106MABX05HD5L,2026-09-26 19:26:50
税,91150502MADBMNHF4K,2026-09-26 19:28:02
税,91419001MA9G502913,2026-09-26 19:28:13
税,91371302MADDKM677C,2026-09-26 19:28:50
税,91310113MAEP2H9856,2026-09-26 19:29:21
税,91330522MA2JJKJA9Q,2026-09-27 19:22:37
税,91460000MA5TQDJN9W,2026-09-27 19:22:43
税,91440101MA5CXA67XF,2026-09-27 19:24:03
税,91341300MA2MWY286H,2026-09-27 19:24:31
税,91340104MA2W0AYDXQ,2026-09-27 19:25:31
税,91310115MAK5H66X5W,2026-09-27 19:26:02
税,91341222MA8PH77G1C,2026-09-29 19:22:07
税,91320113MA7N5EDH87,2026-09-29 19:22:29
税,91330106321911163P,2026-09-29 19:23:11
税,91110108MA006N1B9L,2026-09-29 19:23:56
税,91653125MAC4G19XXP,2026-09-29 19:24:29
税,91410100MA445GU69K,2026-09-29 19:24:35
税,91360802MADNTX9T48,2026-09-29 19:24:50
税,91140105MA0L1F176J,2026-09-29 19:26:14
税,91610113MAE02F7C7F,2026-09-29 19:26:16
税,91520102MABLY8JJXY,2026-09-29 19:26:37
税,91340200325442632T,2026-09-29 19:27:19
税,91361002MAD24W8J8L,2026-09-29 19:27:28
税,91340100MA8QELW53A,2026-09-29 19:27:48
税,91320213MA1Y3NL10L,2026-09-29 19:28:13
税,91320508MAK7LF9P70,2026-09-29 19:29:11
税,91411527MADNKBGX2T,2026-09-29 19:29:40
税,91370105MA949XE3XT,2026-09-29 19:30:40
税,91440300MA5HPJN30G,2026-09-29 19:32:16
税,91360108MABTMM7LX4,2026-09-29 19:32:24
税,913705024930382096,2026-09-29 19:32:30
税,91610112MAEY09R50W,2026-09-29 19:32:35
税,91410103MA463YX84P,2026-09-29 19:32:45
税,91370100MAG14KAA9R,2026-09-29 19:33:56
税,91331002MACL5EXP8F,2026-09-30 19:22:15
税,91410104MADXLF129T,2026-09-30 19:22:52
税,91500106MAD615N528,2026-09-30 19:23:56
税,91370102MAK3BAM035,2026-09-30 19:23:59
税,91360121MAC5NGN33L,2026-09-30 19:24:01
税,91110111MA02MEGF1Y,2026-09-30 19:24:32
税,91510100MAE436EAXQ,2026-09-30 19:24:58
税,91410505MA4813179B,2026-09-30 19:25:09
税,91410728MA460G0T03,2026-09-30 19:25:28
税,91320113MA1YRB6K99,2026-09-30 19:25:38
税,91130606MA0CGAK50K,2026-09-30 19:26:33
税,91420114MABMY7UM2J,2026-09-30 19:27:50
税,91341002MA2XJD6598,2026-09-30 19:29:33
税,91140424MAEFN8185A,2026-09-30 19:30:39
税,91610303MA6XDHC367,2026-09-30 19:33:05
`

// ---------------- 最小 xlsx 写出（仅标准库） ----------------

type sheet struct {
	name    string
	headers []string
	rows    [][]any
	widths  []float64
	hdrRows map[int]bool // rows 中按表头样式输出的行号（0 起）
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
<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">`)
	if len(s.headers) > 0 {
		b.WriteString(`<sheetViews><sheetView workbookViewId="0"><pane ySplit="1" topLeftCell="A2" activePane="bottomLeft" state="frozen"/></sheetView></sheetViews>`)
	}
	if len(s.widths) > 0 {
		b.WriteString("<cols>")
		for i, wd := range s.widths {
			fmt.Fprintf(&b, `<col min="%d" max="%d" width="%.1f" customWidth="1"/>`, i+1, i+1, wd)
		}
		b.WriteString("</cols>")
	}
	b.WriteString("<sheetData>")
	rowNum := 1
	if len(s.headers) > 0 {
		header := make([]any, len(s.headers))
		for i, h := range s.headers {
			header[i] = h
		}
		writeRow(&b, rowNum, header, 1)
		rowNum++
	}
	for i, r := range s.rows {
		style := 2
		if s.hdrRows[i] {
			style = 1
		}
		writeRow(&b, rowNum, r, style)
		rowNum++
	}
	if len(s.rows) == 0 {
		writeRow(&b, rowNum, []any{"(无数据)"}, 2)
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
