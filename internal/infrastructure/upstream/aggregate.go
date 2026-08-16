package upstream

import (
	"encoding/json"
	"fmt"

	"github.com/datahub/relay/internal/domain/model"
)

// 本文件只保留「一个子源的结果 → 合并 range 里的一段」这套归一原语。多源的调用
// 编排已从「并发扇出全部子源」改为「按优先级串行、命中即停」，见 sourcing.go
// (upstream.Sourcer)。

// aggSection 是一个子源归一后的结果，聚合进合并 range 的一段。
type aggSection struct {
	Status string          `json:"status"`          // ok=查得 / empty=查无 / error=该源失败 / skipped=未调用
	Data   json.RawMessage `json:"data,omitempty"`  // 查得时透出子源 result.range 原样
	Error  string          `json:"error,omitempty"` // status=error/skipped 时的原因摘要
}

// classify 把一个子源的 (result, err) 归一为聚合段：err→error / 999→empty /
// 001→ok(透出 range) / 其余非预期码→error。
func classify(res *model.UpstreamResult, err error) aggSection {
	if err != nil {
		return aggSection{Status: model.CallError, Error: err.Error()}
	}
	if res == nil {
		return aggSection{Status: model.CallError, Error: "子源返回空结果"}
	}
	switch res.Code {
	case "999":
		return aggSection{Status: model.CallEmpty}
	case "001":
		sec := aggSection{Status: model.CallOK}
		if res.Range != "" && json.Valid([]byte(res.Range)) {
			sec.Data = json.RawMessage(res.Range)
		}
		return sec
	default:
		return aggSection{Status: model.CallError, Error: fmt.Sprintf("子源返回非预期 code=%s msg=%s", res.Code, res.Msg)}
	}
}
