package harness

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// DefaultTaxNumbersXLSX 是 test/cases/测试税号.xlsx 相对仓库根的路径。
const DefaultTaxNumbersXLSX = "test/cases/测试税号.xlsx"

// LoadTestTaxNumbers 从 测试税号.xlsx 读取税号列表（跳过表头行）。
func LoadTestTaxNumbers() ([]string, error) {
	return LoadTestTaxNumbersResolved()
}

func repoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found from %s", wd)
		}
		dir = parent
	}
}

func readSharedStrings(zr *zip.Reader) ([]string, error) {
	f, err := openZip(zr, "xl/sharedStrings.xml")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := xml.NewDecoder(f)
	var out []string
	var inSI, inT bool
	var cur strings.Builder
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("sharedStrings xml: %w", err)
		}
		switch el := tok.(type) {
		case xml.StartElement:
			switch el.Name.Local {
			case "si":
				inSI = true
				cur.Reset()
			case "t":
				if inSI {
					inT = true
				}
			}
		case xml.EndElement:
			switch el.Name.Local {
			case "si":
				out = append(out, cur.String())
				inSI = false
			case "t":
				inT = false
			}
		case xml.CharData:
			if inT {
				cur.Write(el)
			}
		}
	}
	return out, nil
}

func readSheet(zr *zip.Reader) ([][]string, error) {
	f, err := openZip(zr, "xl/worksheets/sheet1.xml")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := xml.NewDecoder(f)
	var rows [][]string
	var curRow []string
	var curCell strings.Builder
	var cellType string
	inV := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("sheet xml: %w", err)
		}
		switch el := tok.(type) {
		case xml.StartElement:
			switch el.Name.Local {
			case "row":
				curRow = nil
			case "c":
				cellType = attr(el, "t")
				curCell.Reset()
			case "v":
				inV = true
			}
		case xml.EndElement:
			switch el.Name.Local {
			case "row":
				if len(curRow) > 0 {
					rows = append(rows, curRow)
				}
			case "c":
				curRow = append(curRow, curCell.String())
			case "v":
				inV = false
			}
		case xml.CharData:
			if inV {
				val := string(el)
				if cellType == "s" {
					// 共享字符串索引在 parse 阶段再解；此处先占位，后面统一解。
					curCell.WriteString("s:" + val)
				} else {
					curCell.WriteString(val)
				}
			}
		}
	}
	return rows, nil
}

func attr(el xml.StartElement, key string) string {
	for _, a := range el.Attr {
		if a.Name.Local == key {
			return a.Value
		}
	}
	return ""
}

func openZip(zr *zip.Reader, name string) (io.ReadCloser, error) {
	for _, f := range zr.File {
		if f.Name == name {
			return f.Open()
		}
	}
	return nil, fmt.Errorf("%s not found in xlsx", name)
}

// ResolveSharedCell 把 sheet 单元格里的 s:N 索引替换成共享字符串文本。
func ResolveSharedCell(cell string, shared []string) string {
	if strings.HasPrefix(cell, "s:") {
		idx := 0
		fmt.Sscanf(strings.TrimPrefix(cell, "s:"), "%d", &idx)
		if idx >= 0 && idx < len(shared) {
			return shared[idx]
		}
	}
	return cell
}

// LoadTestTaxNumbersResolved 读取 xlsx 并解共享字符串。
func LoadTestTaxNumbersResolved() ([]string, error) {
	root, err := repoRoot()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(root, DefaultTaxNumbersXLSX)
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", DefaultTaxNumbersXLSX, err)
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, err
	}
	shared, err := readSharedStrings(zr)
	if err != nil {
		return nil, err
	}
	rows, err := readSheet(zr)
	if err != nil {
		return nil, err
	}
	var out []string
	for i, row := range rows {
		if i == 0 || len(row) == 0 {
			continue
		}
		cc := strings.TrimSpace(ResolveSharedCell(row[0], shared))
		if cc == "" || cc == "测试税号" {
			continue
		}
		out = append(out, cc)
	}
	return out, nil
}
