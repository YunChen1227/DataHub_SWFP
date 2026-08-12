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
