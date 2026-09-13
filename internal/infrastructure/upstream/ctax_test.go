package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/datahub/relay/internal/domain/model"
)

const ctaxCreditCode = "92500233MA60R5KW8M"

// ctaxStub 起一个按真实服务器形态应答的挡板，并记下上游收到的鉴权头与请求体。
func ctaxStub(t *testing.T, body func(req ctaxRequest) string) (*CTaxClient, *ctaxRequest, *http.Header) {
	t.Helper()
	var got ctaxRequest
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Errorf("请求不是合法 JSON: %v (%s)", err, raw)
		}
		w.Header().Set("Content-Type", "application/json;charset=UTF-8")
		_, _ = io.WriteString(w, body(got))
	}))
	t.Cleanup(srv.Close)
	c := NewCTax(CTaxConfig{
		BaseURL: srv.URL, AppID: "APPID", Token: "TOK", AuthCode: "test",
	}, srv.Client())
	return c, &got, &hdr
}

// ctaxBody 组一份应答；sections 为 data 节点内的业务段 JSON 片段（空则无 data）。
func ctaxBody(code, sections string) string {
	if sections == "" {
		return `{"code":"` + code + `","msg":"msg","orderNo":"ORD-1"}`
	}
	return `{"code":"` + code + `","msg":"msg","orderNo":"ORD-1","data":{` + sections + `}}`
}

const (
	ctaxTaxSectionJSON     = `"sbsj":[{"nsrsbh":"x","ynse":"1300.00"}]`
	ctaxInvoiceSectionJSON = `"kphzxx":[{"nsrsbh":"x","kpje":"888.00"}]`
	ctaxNsrjbxxJSON        = `"nsrjbxx":{"nsrsbh":"x","nsrmc":"某某公司"}`
)

// TestCTaxRequestShape 按真实服务器要求发报文：鉴权走 X-AppId/X-Token 头，业务参数
// 平铺（identityId/type/authTimeBeg/authTimeEnd/authCode），type 随本次请求维度变化。
func TestCTaxRequestShape(t *testing.T) {
	cases := []struct {
		name     string
		want     model.DimSet
		wantType string
	}{
		{"两项", model.AllDims(), ctaxTypeBoth},
		{"仅发票", model.DimSet{Invoice: true}, ctaxTypeInvoice},
		{"仅税务", model.DimSet{Tax: true}, ctaxTypeTax},
		{"未指定(老下游)", model.DimSet{}, ctaxTypeBoth},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, req, hdr := ctaxStub(t, func(ctaxRequest) string {
				return ctaxBody("200", ctaxTaxSectionJSON+","+ctaxInvoiceSectionJSON)
			})
			if _, err := c.Query(context.Background(), &model.UpstreamRequest{
				CreditCode: ctaxCreditCode, Reqid: "r1", Want: tc.want,
			}); err != nil {
				t.Fatalf("Query: %v", err)
			}
			if req.Type != tc.wantType {
				t.Fatalf("type=%q, want %q", req.Type, tc.wantType)
			}
			if req.IdentityID != ctaxCreditCode {
				t.Fatalf("identityId=%q, want %q", req.IdentityID, ctaxCreditCode)
			}
			if req.AuthCode != "test" || req.AuthTimeBeg == "" || req.AuthTimeEnd == "" {
				t.Fatalf("授权信息缺失: %+v", req)
			}
			if hdr.Get(ctaxHeaderAppID) != "APPID" || hdr.Get(ctaxHeaderToken) != "TOK" {
				t.Fatalf("鉴权头缺失: X-AppId=%q X-Token=%q", hdr.Get(ctaxHeaderAppID), hdr.Get(ctaxHeaderToken))
			}
		})
	}
}

// TestCTaxGotFollowsData 综合源的关键断言：实得维度按 data 里真正带回的业务段判定，
// 而不是"请求了两项就算两项"。请求两项只回税务段时必须只报 tax——否则上层会按
// 【发票+税务】档向客户收费，即收没给到的数据的钱。
func TestCTaxGotFollowsData(t *testing.T) {
	cases := []struct {
		name     string
		sections string
		want     model.DimSet
	}{
		{"两维皆有", ctaxTaxSectionJSON + "," + ctaxInvoiceSectionJSON, model.AllDims()},
		{"只有税务段", ctaxNsrjbxxJSON + "," + ctaxTaxSectionJSON, model.DimSet{Tax: true}},
		{"只有发票段", ctaxInvoiceSectionJSON, model.DimSet{Invoice: true}},
		{"业务段为空容器", `"sbsj":[],"kphzxx":null`, model.DimSet{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _, _ := ctaxStub(t, func(ctaxRequest) string { return ctaxBody("200", tc.sections) })
			res, err := c.Query(context.Background(), &model.UpstreamRequest{
				CreditCode: ctaxCreditCode, Reqid: "r2", Want: model.AllDims(),
			})
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			if tc.want.Empty() {
				// 有 data 容器但无业务段：与查无同口径，绝不能当查得计费。
				if res.Code != "999" || !res.Got.Empty() {
					t.Fatalf("空业务段应归一为 999/无实得维度, got %s/%s", res.Code, res.Got)
				}
				return
			}
			if res.Code != "001" || res.Got != tc.want {
				t.Fatalf("want 001/%s, got %s/%s", tc.want, res.Code, res.Got)
			}
			if res.UID != "ORD-1" || res.LogID != "ORD-1" {
				t.Fatalf("orderNo 应落 UID/LogID 供对账, got uid=%q logId=%q", res.UID, res.LogID)
			}
		})
	}
}

// TestCTaxGotCappedByWant 单维请求时实得维度不得超出请求维度：上游即便多回了另一维，
// 我们也不曾把它交付给下游，不能按它计费。
func TestCTaxGotCappedByWant(t *testing.T) {
	c, _, _ := ctaxStub(t, func(ctaxRequest) string {
		return ctaxBody("200", ctaxTaxSectionJSON+","+ctaxInvoiceSectionJSON)
	})
	res, err := c.Query(context.Background(), &model.UpstreamRequest{
		CreditCode: ctaxCreditCode, Reqid: "r3", Want: model.DimSet{Invoice: true},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.Got != (model.DimSet{Invoice: true}) {
		t.Fatalf("单发票请求的实得维度应只含发票, got %s", res.Got)
	}
}

// TestCTaxResultCodes 真实返回码归一：200 有 data → 查得；404 → 查无；
// 203 授权校验失败 → 该源失败（带上游码 + orderNo 可追查）。
func TestCTaxResultCodes(t *testing.T) {
	t.Run("404 查无", func(t *testing.T) {
		c, _, _ := ctaxStub(t, func(ctaxRequest) string { return ctaxBody("404", "") })
		res, err := c.Query(context.Background(), &model.UpstreamRequest{
			CreditCode: ctaxCreditCode, Reqid: "r4", Want: model.AllDims(),
		})
		if err != nil {
			t.Fatalf("查无不应返回 error: %v", err)
		}
		if res.Code != "999" || res.Range != "" {
			t.Fatalf("want 999/空 range, got %s/%q", res.Code, res.Range)
		}
		if res.UID != "ORD-1" {
			t.Fatalf("查无也应带 orderNo: %+v", res)
		}
	})

	t.Run("203 授权校验失败", func(t *testing.T) {
		c, _, _ := ctaxStub(t, func(ctaxRequest) string {
			return `{"code":"203","msg":"授权信息校验失败","orderNo":"ORD-9","data":null}`
		})
		_, err := c.Query(context.Background(), &model.UpstreamRequest{
			CreditCode: ctaxCreditCode, Reqid: "r5", Want: model.AllDims(),
		})
		var ue *model.UpstreamError
		if !errors.As(err, &ue) {
			t.Fatalf("上游业务失败必须回 *model.UpstreamError（否则审计拿不到上游码）, got %T: %v", err, err)
		}
		if ue.Code != "203" || ue.UID != "ORD-9" {
			t.Fatalf("应带上游码 203 与 orderNo, got code=%q uid=%q", ue.Code, ue.UID)
		}
	})
}

// TestCTaxCredentialsRequired 鉴权凭证不完整时立刻报错，不发请求（上游必报 A20029，
// 本地先拦下，避免发出注定失败的请求）。
func TestCTaxCredentialsRequired(t *testing.T) {
	c := NewCTax(CTaxConfig{BaseURL: "http://example.invalid", AppID: "", Token: "TOK"}, http.DefaultClient)
	if _, err := c.Query(context.Background(), &model.UpstreamRequest{
		CreditCode: ctaxCreditCode, Reqid: "r6", Want: model.AllDims(),
	}); err == nil {
		t.Fatal("缺 appId 应返回错误")
	}

	c2 := NewCTax(CTaxConfig{}, http.DefaultClient)
	if _, err := c2.Query(context.Background(), &model.UpstreamRequest{
		CreditCode: ctaxCreditCode, Reqid: "r7", Want: model.AllDims(),
	}); err == nil {
		t.Fatal("未配 baseURL 应返回错误")
	}
}
