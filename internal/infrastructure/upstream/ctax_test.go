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

// ctaxStub 起一个按文档形态应答的挡板，并记下上游收到的入参 (文档 §3 入参表)。
func ctaxStub(t *testing.T, body func(param ctaxParam) string) (*CTaxClient, *ctaxParam, *string) {
	t.Helper()
	var got ctaxParam
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		var env ctaxEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Errorf("请求不是合法 JSON: %v (%s)", err, raw)
		}
		got = env.Param
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		_, _ = io.WriteString(w, body(env.Param))
	}))
	t.Cleanup(srv.Close)
	return NewCTax(CTaxConfig{BaseURL: srv.URL}, srv.Client()), &got, &path
}

// ctaxBody 组一份 result 应答；sections 为 data 节点内的业务段 JSON 片段。
func ctaxBody(code, sections string) string {
	if sections == "" {
		return `{"result":{"code":"` + code + `","msg":"msg"}}`
	}
	return `{"result":{"code":"` + code + `","msg":"msg","data":{` + sections + `}}}`
}

const (
	ctaxTaxSectionJSON     = `"sbsj":[{"nsrsbh":"x","ynse":"1300.00"}]`
	ctaxInvoiceSectionJSON = `"kphzxx":[{"nsrsbh":"x","kpje":"888.00"}]`
	ctaxNsrjbxxJSON        = `"nsrjbxx":{"nsrsbh":"x","nsrmc":"某某公司"}`
)

// TestCTaxTypeFollowsWant 按本次请求维度传 type：多要一维就多付一次上游的钱，而
// 多出来的数据既不透出下游也不计费。路径固定 /c/tax，identity 为社会信用代码。
func TestCTaxTypeFollowsWant(t *testing.T) {
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
			c, param, path := ctaxStub(t, func(ctaxParam) string {
				return ctaxBody(ctaxCodeOK, ctaxTaxSectionJSON+","+ctaxInvoiceSectionJSON)
			})
			if _, err := c.Query(context.Background(), &model.UpstreamRequest{
				CreditCode: ctaxCreditCode, Reqid: "r1", Want: tc.want,
			}); err != nil {
				t.Fatalf("Query: %v", err)
			}
			if param.Type != tc.wantType {
				t.Fatalf("type=%q, want %q", param.Type, tc.wantType)
			}
			if param.Identity != ctaxCreditCode {
				t.Fatalf("identity=%q, want %q", param.Identity, ctaxCreditCode)
			}
			if *path != ctaxPath {
				t.Fatalf("path=%q, want %q", *path, ctaxPath)
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
			c, _, _ := ctaxStub(t, func(ctaxParam) string { return ctaxBody(ctaxCodeOK, tc.sections) })
			res, err := c.Query(context.Background(), &model.UpstreamRequest{
				CreditCode: ctaxCreditCode, Reqid: "r2", Want: model.AllDims(),
			})
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			if tc.want.Empty() {
				// SYS200 但没有任何业务数据：与查无同口径，绝不能当查得计费。
				if res.Code != "999" || !res.Got.Empty() {
					t.Fatalf("空业务段应归一为 999/无实得维度, got %s/%s", res.Code, res.Got)
				}
				return
			}
			if res.Code != "001" || res.Got != tc.want {
				t.Fatalf("want 001/%s, got %s/%s", tc.want, res.Code, res.Got)
			}
		})
	}
}

// TestCTaxGotCappedByWant 单维请求时实得维度不得超出请求维度：上游即便多回了另一维，
// 我们也不曾把它交付给下游，不能按它计费。
func TestCTaxGotCappedByWant(t *testing.T) {
	c, _, _ := ctaxStub(t, func(ctaxParam) string {
		return ctaxBody(ctaxCodeOK, ctaxTaxSectionJSON+","+ctaxInvoiceSectionJSON)
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

// TestCTaxResultCodes 文档 §4 返回码逐个归一：SYS200 查得 / SYS404 查无 /
// 500 异常（该源失败，带业务码可追查）。
func TestCTaxResultCodes(t *testing.T) {
	t.Run("SYS404 查无", func(t *testing.T) {
		c, _, _ := ctaxStub(t, func(ctaxParam) string { return ctaxBody(ctaxCodeEmpty, "") })
		res, err := c.Query(context.Background(), &model.UpstreamRequest{
			CreditCode: ctaxCreditCode, Reqid: "r4", Want: model.AllDims(),
		})
		if err != nil {
			t.Fatalf("查无不应返回 error: %v", err)
		}
		if res.Code != "999" || res.Range != "" {
			t.Fatalf("want 999/空 range, got %s/%q", res.Code, res.Range)
		}
	})

	t.Run("500 异常", func(t *testing.T) {
		c, _, _ := ctaxStub(t, func(ctaxParam) string { return ctaxBody(ctaxCodeError, "") })
		_, err := c.Query(context.Background(), &model.UpstreamRequest{
			CreditCode: ctaxCreditCode, Reqid: "r5", Want: model.AllDims(),
		})
		var ue *model.UpstreamError
		if !errors.As(err, &ue) {
			t.Fatalf("上游业务失败必须回 *model.UpstreamError（否则审计拿不到上游码）, got %T: %v", err, err)
		}
		if ue.Code != ctaxCodeError {
			t.Fatalf("应带上游返回码原值 %s, got %q", ctaxCodeError, ue.Code)
		}
	})
}

// TestCTaxAcceptsBothEnvelopeShapes 文档没给报文示例，故应答的两种形态都要认：
// 外层包一层 result，或直接就是 result 本体。挑一种赌等于赌联调失败。
func TestCTaxAcceptsBothEnvelopeShapes(t *testing.T) {
	c, _, _ := ctaxStub(t, func(ctaxParam) string {
		return `{"code":"` + ctaxCodeOK + `","msg":"查询成功","data":{` + ctaxTaxSectionJSON + `}}`
	})
	res, err := c.Query(context.Background(), &model.UpstreamRequest{
		CreditCode: ctaxCreditCode, Reqid: "r6", Want: model.AllDims(),
	})
	if err != nil {
		t.Fatalf("裸 result 形态应能解析: %v", err)
	}
	if res.Code != "001" || res.Got != (model.DimSet{Tax: true}) {
		t.Fatalf("want 001/tax, got %s/%s", res.Code, res.Got)
	}
}

// TestCTaxBaseURLMissing 未配 baseURL 时立刻报错，不发请求（文档未给地址，配置里
// 是占位符时必须一眼看出来，而不是发出一个诡异的请求）。
func TestCTaxBaseURLMissing(t *testing.T) {
	c := NewCTax(CTaxConfig{}, http.DefaultClient)
	if _, err := c.Query(context.Background(), &model.UpstreamRequest{
		CreditCode: ctaxCreditCode, Reqid: "r7", Want: model.AllDims(),
	}); err == nil {
		t.Fatal("未配 baseURL 应返回错误")
	}
}
