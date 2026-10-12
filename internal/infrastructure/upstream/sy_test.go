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

const syCreditCode = "91440300MA5F0M1Y40"

// 包年接口 data 片段（【税票数据查询-包年】接口文档 表1）。
const (
	syQyxxJSON           = `"success":true,"nsrsbh":"x","qyxxList":[{"nsrsbh":"x","nsrmc":"某某公司"}]`
	syTaxSectionJSON     = `"sbxxList":[{"djxh":"x","ynse":1300.00}]`
	syInvoiceSectionJSON = `"kphzxxList":[{"djxh":"x","kpyf":"202605"}]`
)

// syStub 起一个按旧 sy/tax 协议应答的挡板，记下鉴权头与请求体。
func syStub(t *testing.T, body func(req syRequest) string) (*SyClient, *syRequest, *http.Header, *int) {
	t.Helper()
	var got syRequest
	var hdr http.Header
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		hdr = r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Errorf("请求不是合法 JSON: %v (%s)", err, raw)
		}
		var keys map[string]json.RawMessage
		_ = json.Unmarshal(raw, &keys)
		for _, k := range []string{"identityId", "type", "authTimeBeg", "authTimeEnd", "authCode"} {
			if _, ok := keys[k]; !ok {
				t.Errorf("请求体缺字段 %s: %s", k, raw)
			}
		}
		if _, ok := keys["identity"]; ok {
			t.Errorf("sy 必须用 identityId 而不是 identity: %s", raw)
		}
		w.Header().Set("Content-Type", "application/json;charset=UTF-8")
		_, _ = io.WriteString(w, body(got))
	}))
	t.Cleanup(srv.Close)
	c := NewSy(SyConfig{
		BaseURL: srv.URL, AppID: "APPID", Token: "TOK", AuthCode: "gckj",
	}, srv.Client())
	return c, &got, &hdr, &hits
}

func TestSyRequestShape(t *testing.T) {
	cases := []struct {
		name     string
		want     model.DimSet
		wantType string
	}{
		{"两项", model.AllDims(), ctaxTypeBoth},
		{"仅发票", model.DimSet{Invoice: true}, ctaxTypeInvoice},
		{"仅税务", model.DimSet{Tax: true}, ctaxTypeTax},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, req, hdr, _ := syStub(t, func(syRequest) string {
				return ctaxBody("200", syQyxxJSON)
			})
			if _, err := c.Query(context.Background(), &model.UpstreamRequest{
				CreditCode: syCreditCode, Reqid: "s1", Want: tc.want,
			}); err != nil {
				t.Fatalf("Query: %v", err)
			}
			if req.Type != tc.wantType {
				t.Fatalf("type=%q, want %q", req.Type, tc.wantType)
			}
			if req.IdentityID != syCreditCode {
				t.Fatalf("identityId=%q, want %q", req.IdentityID, syCreditCode)
			}
			if req.AuthCode != "gckj" || req.AuthTimeBeg == "" || req.AuthTimeEnd == "" {
				t.Fatalf("授权信息缺失: %+v", req)
			}
			if hdr.Get(ctaxHeaderAppID) != "APPID" || hdr.Get(ctaxHeaderToken) != "TOK" {
				t.Fatalf("鉴权头缺失: X-AppId=%q X-Token=%q", hdr.Get(ctaxHeaderAppID), hdr.Get(ctaxHeaderToken))
			}
		})
	}
}

func TestSyRejectsEmptyWant(t *testing.T) {
	c, _, _, hits := syStub(t, func(syRequest) string { return ctaxBody("200", syQyxxJSON) })
	if _, err := c.Query(context.Background(), &model.UpstreamRequest{
		CreditCode: syCreditCode, Reqid: "s0",
	}); err == nil {
		t.Fatal("空 Want 应拒绝")
	}
	if *hits != 0 {
		t.Fatalf("空 Want 不得发请求, hits=%d", *hits)
	}
}

func TestSyFoundIgnoresEmptySections(t *testing.T) {
	// 只有企业基本信息、业务段为空：ctax 会当查无，sy 必须当查得，Got = 请求维度。
	c, _, _, _ := syStub(t, func(syRequest) string {
		return ctaxBody("200", syQyxxJSON+`,"sbxxList":[],"zsxxList":null`)
	})
	res, err := c.Query(context.Background(), &model.UpstreamRequest{
		CreditCode: syCreditCode, Reqid: "s2", Want: taxDim,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.Code != "001" || res.Got != taxDim {
		t.Fatalf("200+非空 data 应按请求维度算查得, got %s/%s", res.Code, res.Got)
	}
	if res.UID != "ORD-1" {
		t.Fatalf("orderNo 应落 UID, got %q", res.UID)
	}
}

func TestSyPrunesUnrequestedDims(t *testing.T) {
	c, _, _, _ := syStub(t, func(syRequest) string {
		return ctaxBody("200", syQyxxJSON+","+syTaxSectionJSON+","+syInvoiceSectionJSON)
	})
	res, err := c.Query(context.Background(), &model.UpstreamRequest{
		CreditCode: syCreditCode, Reqid: "s3", Want: taxDim,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.Got != taxDim {
		t.Fatalf("Got 必须是请求维度, got %s", res.Got)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(res.Range), &m); err != nil {
		t.Fatalf("range: %v", err)
	}
	if _, ok := m["kphzxxList"]; ok {
		t.Fatalf("未请求的发票段不应透出: %s", res.Range)
	}
	if _, ok := m["sbxxList"]; !ok {
		t.Fatalf("请求的税务段应保留: %s", res.Range)
	}
	if _, ok := m["qyxxList"]; !ok {
		t.Fatalf("两维共有的企业基本信息应保留: %s", res.Range)
	}
}

func TestSyResultCodes(t *testing.T) {
	t.Run("404 查无", func(t *testing.T) {
		c, _, _, _ := syStub(t, func(syRequest) string { return ctaxBody("404", "") })
		res, err := c.Query(context.Background(), &model.UpstreamRequest{
			CreditCode: syCreditCode, Reqid: "s4", Want: taxDim,
		})
		if err != nil {
			t.Fatalf("查无不应返回 error: %v", err)
		}
		if res.Code != "999" || res.Range != "" {
			t.Fatalf("want 999/空 range, got %s/%q", res.Code, res.Range)
		}
	})
	t.Run("200 空 data 查无", func(t *testing.T) {
		c, _, _, _ := syStub(t, func(syRequest) string {
			return `{"code":"200","msg":"ok","orderNo":"ORD-1","data":null}`
		})
		res, err := c.Query(context.Background(), &model.UpstreamRequest{
			CreditCode: syCreditCode, Reqid: "s5", Want: taxDim,
		})
		if err != nil || res.Code != "999" {
			t.Fatalf("空 data 应 999, got %v / %v", res, err)
		}
	})
	t.Run("success=false 查无", func(t *testing.T) {
		c, _, _, _ := syStub(t, func(syRequest) string {
			return ctaxBody("200", `"success":false,"nsrsbh":"x"`)
		})
		res, err := c.Query(context.Background(), &model.UpstreamRequest{
			CreditCode: syCreditCode, Reqid: "s5b", Want: taxDim,
		})
		if err != nil || res.Code != "999" {
			t.Fatalf("success=false 应 999, got %v / %v", res, err)
		}
	})
	t.Run("文档形态 result 包裹 + SYS200", func(t *testing.T) {
		c, _, _, _ := syStub(t, func(syRequest) string {
			return `{"result":{"code":"SYS200","msg":"查询成功","data":{` + syQyxxJSON + "," + syTaxSectionJSON + `}}}`
		})
		res, err := c.Query(context.Background(), &model.UpstreamRequest{
			CreditCode: syCreditCode, Reqid: "s5c", Want: taxDim,
		})
		if err != nil || res.Code != "001" || res.Got != taxDim {
			t.Fatalf("result 包裹的 SYS200 应查得, got %v / %v", res, err)
		}
	})
	t.Run("203 授权失败", func(t *testing.T) {
		c, _, _, _ := syStub(t, func(syRequest) string {
			return `{"code":"203","msg":"授权信息校验失败","orderNo":"ORD-9","data":null}`
		})
		_, err := c.Query(context.Background(), &model.UpstreamRequest{
			CreditCode: syCreditCode, Reqid: "s6", Want: taxDim,
		})
		var ue *model.UpstreamError
		if !errors.As(err, &ue) {
			t.Fatalf("必须回 *model.UpstreamError, got %T: %v", err, err)
		}
		if ue.Code != "203" || ue.UID != "ORD-9" {
			t.Fatalf("应带上游码 203 与 orderNo, got code=%q uid=%q", ue.Code, ue.UID)
		}
	})
}

func TestSyCredentialsRequired(t *testing.T) {
	c := NewSy(SyConfig{BaseURL: "http://example.invalid", AppID: "", Token: "TOK"}, http.DefaultClient)
	if _, err := c.Query(context.Background(), &model.UpstreamRequest{
		CreditCode: syCreditCode, Reqid: "s7", Want: taxDim,
	}); err == nil {
		t.Fatal("缺 appId 应返回错误")
	}
	c2 := NewSy(SyConfig{}, http.DefaultClient)
	if _, err := c2.Query(context.Background(), &model.UpstreamRequest{
		CreditCode: syCreditCode, Reqid: "s8", Want: taxDim,
	}); err == nil {
		t.Fatal("未配 baseURL 应返回错误")
	}
}
