# -*- coding: utf-8 -*-
"""把重查结果写回对账单副本：新增明细sheet + 金额为0 sheet + 在对账单统计页加一条。"""
import sys
from decimal import Decimal, InvalidOperation

from openpyxl import load_workbook
from openpyxl.styles import Alignment, Border, Font, PatternFill, Side
from openpyxl.utils import get_column_letter

sys.stdout.reconfigure(encoding="utf-8")

TYPE_LABEL = {"invoice": "票", "tax": "税", "both": "票税"}

HEADERS = [
    "税号", "企业名称", "计费维度", "所属明细块", "查询结果", "是否查得", "实得维度",
    "近12月开票金额", "近12月应税销售收入", "近12月应纳税额", "近12月实缴金额",
    "近12月税务金额", "近12月总金额", "近12月总金额是否为0", "说明",
    "bodyCode", "reqid", "耗时秒",
]
WIDTHS = [22, 30, 10, 10, 10, 10, 12, 16, 18, 16, 16, 16, 16, 18, 30, 10, 20, 8]


def dec(v):
    if v in (None, ""):
        return None
    try:
        return Decimal(str(v))
    except InvalidOperation:
        return None


def money(v):
    d = dec(v)
    return float(d) if d is not None else ""


def yn(v):
    return "" if v is None else ("是" if v else "否")


def got_label(rec):
    if rec.get("outcome") != "查得":
        return ""
    gi, gt = rec.get("gotInvoice"), rec.get("gotTax")
    if gi and gt:
        return "票税"
    if gi:
        return "票"
    if gt:
        return "税"
    return ""


def advice(rec):
    o = rec.get("outcome")
    if o != "查得":
        return {"查无": "查无数据", "未取得": "未取得有效数据", "失败": "调用失败：" + (rec.get("errorMsg") or "")}.get(o, o)
    dt = rec.get("dataType")
    if dt == "invoice":
        return "近12月发票金额≤0" if rec.get("isZero") else "近12月发票金额>0"
    if dt == "tax":
        return "近12月税务金额≤0" if rec.get("isZero") else "近12月税务金额>0"
    iz, tz = rec.get("invZero"), rec.get("taxZero")
    if iz and tz:
        return "发票、税务近12月金额均≤0"
    if iz:
        return "仅发票近12月金额≤0（税务>0）"
    if tz:
        return "仅税务近12月金额≤0（发票>0）"
    return "发票、税务近12月金额均>0"


def row_of(rec):
    return [
        rec["credit"], rec.get("company") or rec.get("name") or "",
        TYPE_LABEL.get(rec["dataType"], rec["dataType"]), rec.get("block") or "",
        rec.get("outcome"), "查得" if rec.get("outcome") == "查得" else "未查得",
        got_label(rec),
        money(rec.get("invAmt")), money(rec.get("ysxssr")), money(rec.get("ynse")),
        money(rec.get("sjje")), money(rec.get("taxAmt")), money(rec.get("total")),
        yn(rec.get("isZero")), advice(rec),
        rec.get("bodyCode") or "", rec.get("reqid") or "", rec.get("elapsed") or "",
    ]


HEAD_FONT = Font(bold=True, color="FFFFFF")
HEAD_FILL = PatternFill("solid", fgColor="1F4E79")
ZERO_FILL = PatternFill("solid", fgColor="FCE4D6")
NOTFOUND_FILL = PatternFill("solid", fgColor="EDEDED")
thin = Side(style="thin", color="BFBFBF")
BORD = Border(left=thin, right=thin, top=thin, bottom=thin)


def add_sheet(wb, title, rows, highlight=True):
    if title in wb.sheetnames:
        del wb[title]
    ws = wb.create_sheet(title)
    ws.append(HEADERS)
    for c in ws[1]:
        c.font = HEAD_FONT
        c.fill = HEAD_FILL
        c.alignment = Alignment(horizontal="center", vertical="center")
        c.border = BORD
    for rec in rows:
        ws.append(row_of(rec))
        last = ws.max_row
        fill = None
        if highlight and rec.get("outcome") == "查得" and rec.get("isZero"):
            fill = ZERO_FILL
        elif highlight and rec.get("outcome") != "查得":
            fill = NOTFOUND_FILL
        for c in ws[last]:
            c.border = BORD
            if fill:
                c.fill = fill
    for i, w in enumerate(WIDTHS, 1):
        ws.column_dimensions[get_column_letter(i)].width = w
    ws.freeze_panes = "A2"
    ws.auto_filter.ref = f"A1:{get_column_letter(len(HEADERS))}{ws.max_row}"
    return ws


def write_excel(tasks, records):
    wb = load_workbook(SRC_FOR_COPY)
    by = {r["credit"]: r for r in records}
    ordered = [by[t["credit"]] for t in tasks if t["credit"] in by]

    found = [r for r in ordered if r.get("outcome") == "查得"]
    zero = [r for r in found if r.get("isZero")]
    notfound = [r for r in ordered if r.get("outcome") != "查得"]
    cnt = lambda o: sum(1 for r in ordered if r.get("outcome") == o)

    add_sheet(wb, "近12月金额为0", zero)
    add_sheet(wb, "近12月核查明细", ordered)

    # ---- 对账单统计页追加一块 ----
    sm = wb["对账单"]
    start = 12
    bold = Font(bold=True)
    title_font = Font(bold=True, color="FFFFFF")
    title_fill = PatternFill("solid", fgColor="1F4E79")
    head_fill = PatternFill("solid", fgColor="D6DCE4")
    lines = [
        ("近12月金额核查（账户 j92eme33uipj，窗口 2025-10 至 2026-10）", ""),
        ("计费税号去重数", len(ordered)),
        ("查得", len(found)),
        ("未查得（查无/未取得/失败）", len(notfound)),
        ("　└ 查无", cnt("查无")),
        ("　└ 未取得", cnt("未取得")),
        ("　└ 调用失败", cnt("失败")),
        ("近12月总金额为0的税号（查得且总金额≤0）", len(zero)),
        ("　└ 其中仅票为0", sum(1 for r in zero if r["dataType"] == "invoice")),
        ("　└ 其中仅税为0", sum(1 for r in zero if r["dataType"] == "tax")),
        ("　└ 其中票税均为0", sum(1 for r in zero if r["dataType"] == "both")),
        ("说明", "维度跟随账单计费维度；总金额=票(开票金额)+税(应税收入/应纳税额/实缴金额正数部分)。详见「近12月核查明细」「近12月金额为0」。"),
    ]
    for i, (k, v) in enumerate(lines):
        r = start + i
        ck = sm.cell(r, 2, k)
        cv = sm.cell(r, 3, v)
        if i == 0:
            ck.font = title_font
            ck.fill = title_fill
            cv.fill = title_fill
        elif k in ("计费税号去重数", "查得", "未查得（查无/未取得/失败）", "近12月总金额为0的税号（查得且总金额≤0）"):
            ck.font = bold
            ck.fill = head_fill
            cv.font = bold
    # 高亮关键行
    for r in range(start, start + len(lines)):
        if sm.cell(r, 2).value and "总金额为0的税号" in str(sm.cell(r, 2).value):
            sm.cell(r, 3).fill = ZERO_FILL
            sm.cell(r, 3).font = Font(bold=True, color="C00000")

    wb.save(OUT_PATH)
    print("saved", OUT_PATH)
    print(f"计费税号 {len(ordered)} | 查得 {len(found)} 未查得 {len(notfound)} "
          f"(查无{cnt('查无')}/未取得{cnt('未取得')}/失败{cnt('失败')}) | 近12月金额为0 {len(zero)}")
    print(f"  金额为0拆分: 仅票{sum(1 for r in zero if r['dataType']=='invoice')} "
          f"仅税{sum(1 for r in zero if r['dataType']=='tax')} "
          f"票税{sum(1 for r in zero if r['dataType']=='both')}")


# paths injected by caller
from ganchen_zero import SRC as SRC_FOR_COPY, OUT as OUT_PATH  # noqa: E402
