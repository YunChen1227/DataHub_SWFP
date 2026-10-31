package xlsx

import (
	"archive/zip"
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestWriteWorkbook(t *testing.T) {
	wb := &Workbook{}
	sum := wb.Add("汇总", nil, [][]any{{"客户", "A&B<C>"}, {"类型", "次数"}, {"票", 3}, {"税", 1.5}}, []float64{20, 10})
	sum.HeaderRows = map[int]bool{1: true}
	wb.Add("票明细/超长名称超长名称超长名称超长名称超长名称超长名称超长", []string{"时间", "金额"}, nil, nil)

	var buf bytes.Buffer
	if err := wb.Write(&buf); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("不是合法 zip: %v", err)
	}
	files := map[string]string{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		files[f.Name] = string(b)
	}
	for _, name := range []string{"[Content_Types].xml", "xl/workbook.xml", "xl/worksheets/sheet1.xml", "xl/worksheets/sheet2.xml"} {
		if _, ok := files[name]; !ok {
			t.Fatalf("缺少 %s", name)
		}
	}
	s1 := files["xl/worksheets/sheet1.xml"]
	if !strings.Contains(s1, "A&amp;B&lt;C&gt;") {
		t.Error("字符串未转义")
	}
	if !strings.Contains(s1, "<v>3</v>") || !strings.Contains(s1, "<v>1.5</v>") {
		t.Error("数字应写成数值单元格")
	}
	wbXML := files["xl/workbook.xml"]
	if !strings.Contains(wbXML, `name="汇总"`) {
		t.Error("缺少汇总 sheet 名")
	}
	if strings.Contains(wbXML, "票明细/") {
		t.Error("sheet 名里的非法字符应被替换")
	}
}
