# -*- coding: utf-8 -*-
import sys
sys.stdout.reconfigure(encoding="utf-8")
from openpyxl import load_workbook

FILES = {
    "2026-09": r"c:\Users\XMMXYY\Downloads\下游账单-北京联信征信咨询有限责任公司-2026-09-01至2026-09-30.xlsx",
    "2026-10": r"c:\Users\XMMXYY\Downloads\下游账单-北京联信征信咨询有限责任公司-2026-10-01至2026-10-31.xlsx",
}
found = {}
for m, p in FILES.items():
    wb = load_workbook(p, read_only=True, data_only=True)
    ws = wb["税号明细"]
    it = ws.iter_rows(values_only=True); hdr = next(it)
    idx = {h: i for i, h in enumerate(hdr)}
    d = {}
    for row in it:
        if not row or not row[idx["税号"]]:
            continue
        if str(row[idx["查得情况"]] or "").strip() == "查得":
            d[str(row[idx["税号"]]).strip()] = str(row[idx["请求维度"]] or "").strip()
    found[m] = d
a, b = found["2026-09"], found["2026-10"]
both = set(a) & set(b)
union = set(a) | set(b)
print("sep", len(a), "oct", len(b), "overlap", len(both), "union", len(union))
conflict = [(k, a[k], b[k]) for k in both if a[k] != b[k]]
print("请求维度 conflicts on overlap:", len(conflict))
for c in conflict[:20]:
    print("  ", c)
