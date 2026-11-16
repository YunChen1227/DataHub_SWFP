# -*- coding: utf-8 -*-
"""Extract 查得 tax IDs and their 请求维度 from the Sept & Oct Lianxin bills."""
import sys
sys.stdout.reconfigure(encoding="utf-8")
from openpyxl import load_workbook

FILES = {
    "2026-09": r"c:\Users\XMMXYY\Downloads\下游账单-北京联信征信咨询有限责任公司-2026-09-01至2026-09-30.xlsx",
    "2026-10": r"c:\Users\XMMXYY\Downloads\下游账单-北京联信征信咨询有限责任公司-2026-10-01至2026-10-31.xlsx",
}


def rows(ws):
    it = ws.iter_rows(values_only=True)
    header = next(it)
    idx = {h: i for i, h in enumerate(header)}
    for row in it:
        if not row or not row[idx.get("税号")]:
            continue
        yield idx, row


for month, path in FILES.items():
    wb = load_workbook(path, read_only=True, data_only=True)
    ws = wb["税号明细"]
    found = []  # 查得
    reqset = {}
    gotset = {}
    conc = {}
    for idx, row in rows(ws):
        cc = str(row[idx["税号"]]).strip()
        got = str(row[idx["查得情况"]] or "").strip()
        req = str(row[idx["请求维度"]] or "").strip()
        sd = str(row[idx["实得维度"]] or "").strip()
        concl = str(row[idx["结论"]] or "").strip()
        if got == "查得":
            found.append(cc)
            reqset[cc] = req
            gotset[cc] = sd
            conc[cc] = concl
    # cross-check against 待人工 sheet
    ws2 = wb["数据库无记录（待人工）"]
    manual = set()
    for idx, row in rows(ws2):
        manual.add(str(row[idx["税号"]]).strip())
    from collections import Counter
    print("#" * 70)
    print(month, "查得 tax IDs:", len(found), "unique:", len(set(found)))
    print("  请求维度分布:", dict(Counter(reqset.values())))
    print("  实得维度分布:", dict(Counter(gotset.values())))
    print("  结论分布:", dict(Counter(conc.values())))
    print("  待人工 sheet count:", len(manual))
    print("  查得 not in 待人工:", len(set(found) - manual))
    print("  待人工 not in 查得:", len(manual - set(found)))
