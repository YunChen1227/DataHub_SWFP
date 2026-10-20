//go:build ignore

// 06_admin_crud: 管理后台全流程——登录(对/错)、建用户(返回 secret)、查/列、改
// (SUSPENDED)、轮换密钥(旧签失败/新签成功)、删、审计(过滤+PII 掩码)、无 token 401。
// 临时用户用完即删。
//
// Run: go run test/cases/06_admin_crud.go
package main

import (
	"net/http"
	"strings"

	"github.com/datahub/relay/test/harness"
)

func base() map[string]string {
	return map[string]string{"creditCode": "92500233MA60R5KW8M"}
}

func main() {
	rec := harness.NewRecorder("06_admin_crud", "管理后台全流程")
	defer rec.Finish()

	// 1. 登录（正确）
	token, raw := harness.AdminLogin()
	rec.Check("登录(正确)", "返回 token", token != "", raw)
	if token == "" {
		return
	}
	auth := harness.AuthHeader(token)
	adminBase := harness.AdminBase("swfp")

	// 2. 登录（错误密码）-> 401
	stw, _, wr := harness.Call(http.MethodPost, "/admin/api/login",
		map[string]string{"username": harness.AdminUser, "password": "wrong-pass"}, nil)
	rec.Check("登录(错误密码)", "HTTP 401", stw == 401, "status="+itoa(stw)+" "+wr)

	// 3. 无 token 访问 -> 401
	stn, _, _ := harness.Call(http.MethodGet, adminBase+"/users", nil, nil)
	rec.Check("无 token 访问用户列表", "HTTP 401", stn == 401, "status="+itoa(stn))

	// 4. 建用户
	st, m, cr := harness.Call(http.MethodPost, adminBase+"/users",
		map[string]any{"name": "admin-crud-临时", "mobile": "13800001111"}, auth)
	user, _ := m["user"].(map[string]any)
	licenseID, _ := user["licenseId"].(string)
	appKey, _ := user["appKey"].(string)
	secret1, _ := m["secret"].(string)
	rec.Check("建用户", "返回 user + 一次性 secret", st == 200 && appKey != "" && secret1 != "", cr)
	if licenseID == "" {
		return
	}
	defer harness.Call(http.MethodDelete, adminBase+"/users/"+licenseID, nil, auth)

	// 5. 查单个
	stg, gm, gr := harness.Call(http.MethodGet, adminBase+"/users/"+licenseID, nil, auth)
	gotKey, _ := gm["appKey"].(string)
	rec.Check("查单个用户", "返回该用户 appKey", stg == 200 && gotKey == appKey, gr)

	// 6. 列表
	_, lm, lr := harness.Call(http.MethodGet, adminBase+"/users", nil, auth)
	users, _ := lm["users"].([]any)
	rec.Check("用户列表", "包含至少 1 个用户", len(users) > 0, lr)

	// 6b. 接口开关：新建用户发票、税务默认都关，任何请求都返回 1009。
	dims, _ := gm["allowInvoice"].(bool)
	dimt, _ := gm["allowTax"].(bool)
	rec.Check("新建用户接口开关", "allowInvoice=false allowTax=false", !dims && !dimt, gr)
	rClosed := harness.QuerySWFP(appKey, secret1, base(), nil)
	rec.Check("未开通接口时请求", "errorCode=505007", rClosed.ErrorCode == "505007", rClosed.Raw)

	// 只开税：票税（含不传 dataType）被拒，税务放行。
	stt, tm, tr := harness.Call(http.MethodPatch, adminBase+"/users/"+licenseID,
		map[string]any{"allowTax": true}, auth)
	onlyTax, _ := tm["allowTax"].(bool)
	invOff, _ := tm["allowInvoice"].(bool)
	rec.Check("只开通税务", "allowTax=true allowInvoice=false", stt == 200 && onlyTax && !invOff, tr)
	rBoth := harness.QuerySWFP(appKey, secret1, base(), nil)
	rec.Check("只开税时请求票税", "errorCode=505007 且提示仅允许税务",
		rBoth.ErrorCode == "505007" && strings.Contains(rBoth.Raw, "仅允许访问税务接口"), rBoth.Raw)
	taxOnly := base()
	taxOnly["dataType"] = "tax"
	rTax := harness.QuerySWFP(appKey, secret1, taxOnly, nil)
	rec.Check("只开税时请求税务", "errorCode=0", rTax.ErrorCode == "0", rTax.Raw)

	// 两项都开 = 开通票税，后续老流程照常。
	sto, om, or := harness.Call(http.MethodPatch, adminBase+"/users/"+licenseID,
		map[string]any{"allowInvoice": true, "allowTax": true}, auth)
	oi, _ := om["allowInvoice"].(bool)
	ot, _ := om["allowTax"].(bool)
	rec.Check("开通发票+税务", "allowInvoice=true allowTax=true", sto == 200 && oi && ot, or)

	// 7. 轮换密钥：旧 secret 应失败，新 secret 应成功
	r := harness.QuerySWFP(appKey, secret1, base(), nil)
	rec.Check("轮换前旧 secret 可用", "errorCode=0", r.ErrorCode == "0", r.Raw)

	_, rm, rr := harness.Call(http.MethodPost, adminBase+"/users/"+licenseID+"/rotate-secret", nil, auth)
	secret2, _ := rm["secret"].(string)
	rec.Check("轮换密钥返回新 secret", "secret 非空且不同于旧值", secret2 != "" && secret2 != secret1, rr)

	if secret2 != "" {
		rOld := harness.QuerySWFP(appKey, secret1, base(), nil)
		rec.Check("旧 secret 轮换后失效", "errorCode=505002", rOld.ErrorCode == "505002", rOld.Raw)
		rNew := harness.QuerySWFP(appKey, secret2, base(), nil)
		rec.Check("新 secret 生效", "errorCode=0", rNew.ErrorCode == "0", rNew.Raw)
	}

	// 8. 改：停用
	stp, pm, pr := harness.Call(http.MethodPatch, adminBase+"/users/"+licenseID,
		map[string]any{"status": "SUSPENDED"}, auth)
	gotStatus, _ := pm["status"].(string)
	rec.Check("更新用户(停用)", "status=SUSPENDED", stp == 200 && gotStatus == "SUSPENDED", pr)

	// 8b. 改：三档合同价。rates 整份覆盖，某档 0 表示该档走全局缺省费率。
	strt, rtm, rtr := harness.Call(http.MethodPatch, adminBase+"/users/"+licenseID,
		map[string]any{"rates": map[string]any{"bothFen": 888, "invoiceFen": 555, "taxFen": 0}}, auth)
	rates, _ := rtm["rates"].(map[string]any)
	both, _ := rates["bothFen"].(float64)
	inv, _ := rates["invoiceFen"].(float64)
	tax, _ := rates["taxFen"].(float64)
	rec.Check("更新用户(合同价)", "bothFen=888 invoiceFen=555 taxFen=0",
		strt == 200 && both == 888 && inv == 555 && tax == 0, rtr)

	// 只改 status 时不应把已设的合同价冲掉（rates 缺省 = 不改）。
	_, km, kr := harness.Call(http.MethodPatch, adminBase+"/users/"+licenseID,
		map[string]any{"status": "ACTIVE"}, auth)
	kept, _ := km["rates"].(map[string]any)
	keptBoth, _ := kept["bothFen"].(float64)
	rec.Check("合同价不被无关更新冲掉", "bothFen 仍为 888", keptBoth == 888, kr)
	ki, _ := km["allowInvoice"].(bool)
	kt, _ := km["allowTax"].(bool)
	rec.Check("接口开关不被无关更新冲掉", "allowInvoice=true allowTax=true", ki && kt, kr)

	// 9. 审计：按 appKey 过滤 + PII 掩码（用 demo 主账户，确保有记录）
	_, am, ar := harness.Call(http.MethodGet, adminBase+"/audits?appKey="+harness.AppKey+"&limit=200", nil, auth)
	audits, _ := am["audits"].([]any)
	masked := false
	for _, a := range audits {
		rc, _ := a.(map[string]any)
		if nm, _ := rc["idCardMask"].(string); strings.Contains(nm, "*") {
			masked = true
			break
		}
	}
	rec.Check("审计列表(按 appKey 过滤)", "返回记录数组", am["audits"] != nil, ar)
	rec.Check("审计 PII 掩码", "idCardMask 含 *", masked, "未发现掩码记录(可能本次无 demo 流量)")

	// 9b. 用量统计：按日分桶 + 按用户检索 + 按源下钻（§16.4）。
	stst, sm, sr := harness.Call(http.MethodGet, adminBase+"/stats?granularity=day&limit=50", nil, auth)
	stats, _ := sm["stats"].([]any)
	rec.Check("用量统计(按日)", "HTTP 200 且返回 stats 数组", stst == 200 && sm["stats"] != nil, sr)

	// 每行必须带桶标签与两个计数，且 success <= total（口径自洽）。无流量时空数组也算通过。
	shape := true
	for _, s := range stats {
		row, _ := s.(map[string]any)
		bucket, _ := row["bucket"].(string)
		total, _ := row["total"].(float64)
		success, _ := row["success"].(float64)
		if len(bucket) != 10 || success > total { // 日粒度桶标签形如 2026-09-27
			shape = false
			break
		}
	}
	rec.Check("用量统计行形态", "bucket=YYYY-MM-DD 且 success<=total", shape, sr)

	// 按年分桶：桶标签应为 4 位年份。
	_, ym, yr := harness.Call(http.MethodGet, adminBase+"/stats?granularity=year", nil, auth)
	yStats, _ := ym["stats"].([]any)
	yShape := true
	for _, s := range yStats {
		row, _ := s.(map[string]any)
		if bucket, _ := row["bucket"].(string); len(bucket) != 4 {
			yShape = false
			break
		}
	}
	rec.Check("用量统计(按年)", "bucket=YYYY", yShape, yr)

	// 检索不存在的用户 -> 空结果（而非退化为全量）。
	_, nm2, nr := harness.Call(http.MethodGet, adminBase+"/stats?q=__no_such_user__", nil, auth)
	noneStats, _ := nm2["stats"].([]any)
	rec.Check("用量统计(检索无命中)", "stats 为空数组", nm2["stats"] != nil && len(noneStats) == 0, nr)

	// 按源下钻：不限区间即全时段分布，回答「哪个源查成功的」。
	stsrc, srcm, srcr := harness.Call(http.MethodGet,
		adminBase+"/stats/sources?appKey="+harness.AppKey, nil, auth)
	sources, _ := srcm["sources"].([]any)
	rec.Check("按源统计", "HTTP 200 且返回 sources 数组", stsrc == 200 && srcm["sources"] != nil, srcr)

	// calls = ok+empty+err+skipped，四种状态必须把明细行分完（口径自洽）。
	srcShape := true
	for _, s := range sources {
		row, _ := s.(map[string]any)
		calls, _ := row["calls"].(float64)
		ok, _ := row["ok"].(float64)
		empty, _ := row["empty"].(float64)
		errc, _ := row["err"].(float64)
		skipped, _ := row["skipped"].(float64)
		if calls != ok+empty+errc+skipped {
			srcShape = false
			break
		}
	}
	rec.Check("按源统计口径自洽", "calls = ok+empty+err+skipped", srcShape, srcr)

	// 缺 appKey -> 400（下钻必须限定用户，否则跨用户混算）。
	stbad, _, badr := harness.Call(http.MethodGet, adminBase+"/stats/sources", nil, auth)
	rec.Check("按源统计(缺 appKey)", "HTTP 400", stbad == 400, "status="+itoa(stbad)+" "+badr)

	// 10. 删除
	std, _, dr := harness.Call(http.MethodDelete, adminBase+"/users/"+licenseID, nil, auth)
	rec.Check("删除用户", "HTTP 200", std == 200, dr)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
