# -*- coding: utf-8 -*-
import zipfile
import xml.etree.ElementTree as ET
from pathlib import Path

NS = {"m": "http://schemas.openxmlformats.org/spreadsheetml/2006/main"}

def col_row(ref):
    col = "".join(ch for ch in ref if ch.isalpha())
    return col

def load(path: Path):
    print("FILE", path.name)
    with zipfile.ZipFile(path) as z:
        names = z.namelist()
        shared = []
        if "xl/sharedStrings.xml" in names:
            root = ET.fromstring(z.read("xl/sharedStrings.xml"))
            for si in root.findall("m:si", NS):
                texts = [t.text or "" for t in si.findall(".//m:t", NS)]
                shared.append("".join(texts))
        print("SHARED", len(shared))
        for i, s in enumerate(shared):
            print(f"  s[{i}]={s}")
        sheets = [n for n in names if n.startswith("xl/worksheets/sheet") and n.endswith(".xml")]
        for sh in sheets:
            data = z.read(sh)
            if not data.strip():
                print("---", sh, "EMPTY")
                continue
            root = ET.fromstring(data)
            rows = root.findall("m:sheetData/m:row", NS)
            print("---", sh, "rows", len(rows))
            for row in rows[:80]:
                cells = []
                for c in row.findall("m:c", NS):
                    ref = c.attrib.get("r", "")
                    t = c.attrib.get("t", "")
                    v = c.find("m:v", NS)
                    is_el = c.find("m:is", NS)
                    val = ""
                    if v is not None and v.text:
                        val = v.text
                        if t == "s" and val.isdigit():
                            val = shared[int(val)]
                    elif is_el is not None:
                        texts = [t.text or "" for t in is_el.findall(".//m:t", NS)]
                        val = "".join(texts)
                    cells.append(f"{col_row(ref)}={val}")
                print(" | ".join(cells))
            if len(rows) > 80:
                print("... total", len(rows))

root = Path(r"c:\workspace\DataHub_SWFP")
for p in [
    root / "test" / "cases" / "测试税号.xlsx",
]:
    load(p)
