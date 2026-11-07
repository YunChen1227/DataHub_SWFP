# -*- coding: utf-8 -*-
"""One-off: downstream reconciliation workbook counted by tax number, not by query."""
import json
import sys
from collections import defaultdict
from datetime import datetime
from decimal import Decimal
from pathlib import Path

from openpyxl import Workbook
from openpyxl.styles import Alignment, Border, Font, PatternFill, Side
from openpyxl.utils import get_column_letter

sys.stdout.reconfigure(encoding="utf-8")

SRC = Path(r"c:\Users\XMMXYY\Downloads\下游账单-北京联信征信咨询有限责任公司-2026-09-01至2026-10-31.xlsx")
PROGRESS = Path(r"c:\workspace\DataHub_SWFP\.scratch\lianxin_recall\progress.jsonl")
OUT = Path(r"c:\Users\XMMXYY\Downloads\下游账单-北京联信征信咨询有限责任公司-2026-09-01至2026-10-31-按税号对账.xlsx")

TYPE_LABEL = {"invoice": "票", "tax": "税", "invoice+tax": "票税"}
ORDER = ["invoice", "tax", "invoice+tax"]


def obtained(rec):
    dt = rec.get("dataType")
    gi, gt = rec.get("gotInvoice"), rec.get("gotTax")
    if dt == "invoice":
        return bool(gi)
    if dt == "tax":
        return bool(gt)
    if dt == "both":
        return bool(gi) or bool(gt)
    return False


def load_progress():
    last = {}
    for line in PROGRESS.read_text(encoding="utf-8").splitlines():
        if not line.strip():
            continue
        rec = json.loads(line)
        last[rec["credit"]] = rec
    return last


def load_bill():
    from openpyxl import load_workbook

    wb = load_workbook(SRC, read_only=True, data_only=True)
    by = defaultdict(list)
    for sheet in ("票明细", "税明细", "票税明细"):
        ws = wb[sheet]
        rows = ws.iter_rows(values_only=True)
        header = next(rows)
        idx = {h: i for i, h in enumerate(header)}
        for row in rows:
            if not row or not row[idx["税号"]]:
                continue
            by[str(row[idx["税号"]]).strip()].append(
                {
                    "time": row[idx["时间"]],
                    "req": str(row[idx["请求维度"]] or "").strip(),
                    "fee": str(row[idx["计费标识"]] or "").strip(),
                    "std": str(row[idx["计费系统收费标准"]] or "").strip(),
                }
            )
    return by


def money(v):
    if v is None or v == "":
        return None
    return float(Decimal(v))


def classify(calls, rec):
    reqs = {c["req"] for c in calls}
    if len(reqs) != 1:
        raise SystemExit(f"多个请求维度: {calls[0]} {reqs}")
    req = next(iter(reqs))
    outcome = rec.get("outcome")
    if outcome == "查得" and rec.get("bodyCode") == "002" and not obtained(rec):
        outcome = "未取得"
    if outcome == "查得" and rec.get("isZero") is False:
        verdict = "查得计费"
    elif outcome == "查得" and rec.get("isZero") is True:
        verdict = "查得不计费"
    elif outcome == "查无":
        verdict = "未查得"
    elif outcome == "未取得":
        verdict = "未查得"
    else:
        verdict = "未查得"
    note = ""
    if req == "invoice+tax" and outcome == "查得":
        if rec.get("invZero") and not rec.get("taxZero"):
            note = "发票近12月金额为0，税务金额大于0，建议按单税计"
        elif rec.get("taxZero") and not rec.get("invZero"):
            note = "税务近12月金额为0，发票金额大于0，建议按单票计"
        elif rec.get("invZero") and rec.get("taxZero"):
            note = "发票、税务近12月金额均为0"
    if outcome == "查无":
        note = rec.get("bodyMsg") or "查无结果"
    elif outcome == "未取得":
        note = rec.get("bodyMsg") or "未取得数据"
    elif verdict == "查得不计费" and not note:
        note = "近12月总金额<=0"
    return {
        "credit": rec["credit"],
        "company": rec.get("company") or "",
        "req": req,
        "queries": len(calls),
        "charged": sum(1 for c in calls if c["fee"] == "已计费"),
        "free": sum(1 for c in calls if c["fee"] == "免费期"),
        "outcome": outcome,
        "verdict": verdict,
        "inv": money(rec.get("invAmt")) if outcome == "查得" else None,
        "tax": money(rec.get("taxAmt")) if outcome == "查得" else None,
        "total": money(rec.get("total")) if outcome == "查得" else None,
        "inv_zero": ("是" if rec.get("invZero") else "否") if outcome == "查得" and rec.get("invZero") is not None else "",
        "tax_zero": ("是" if rec.get("taxZero") else "否") if outcome == "查得" and rec.get("taxZero") is not None else "",
        "note": note,
        "body_code": rec.get("bodyCode") or "",
        "body_msg": rec.get("bodyMsg") or "",
    }


NAVY = "1F4E79"
HEAD_FILL = PatternFill("solid", fgColor=NAVY)
HEAD_FONT = Font(bold=True, color="FFFFFF")
TITLE_FONT = Font(bold=True, size=14, color=NAVY)
SECTION_FONT = Font(bold=True, color="FFFFFF")
SECTION_FILL = PatternFill("solid", fgColor=NAVY)
LABEL_FILL = PatternFill("solid", fgColor="D6DCE4")
TOTAL_FILL = PatternFill("solid", fgColor="FFF2CC")
ZEBRA = PatternFill("solid", fgColor="F7F9FC")
THIN = Border(
    left=Side(style="thin", color="D9D9D9"),
    right=Side(style="thin", color="D9D9D9"),
    top=Side(style="thin", color="D9D9D9"),
    bottom=Side(style="thin", color="D9D9D9"),
)


def style_header(ws, ncol):
    for cell in ws[1]:
        cell.font = HEAD_FONT
        cell.fill = HEAD_FILL
        cell.alignment = Alignment(horizontal="center", vertical="center", wrap_text=True)
    ws.row_dimensions[1].height = 30
    ws.freeze_panes = "A2"
    ws.auto_filter.ref = f"A1:{get_column_letter(ncol)}{ws.max_row}"
    for row in ws.iter_rows(min_row=2, max_row=ws.max_row, max_col=ncol):
        for cell in row:
            cell.border = THIN
            cell.alignment = Alignment(vertical="center")
        if row[0].row % 2 == 0:
            for cell in row:
                if cell.fill.fgColor is None or cell.fill.fgColor.rgb in (None, "00000000"):
                    cell.fill = ZEBRA


def set_widths(ws, widths):
    for i, w in enumerate(widths, 1):
        ws.column_dimensions[get_column_letter(i)].width = w


def add_detail(wb, title, rows, headers, widths):
    ws = wb.create_sheet(title)
    ws.append(headers)
    for r in rows:
        ws.append(r)
    style_header(ws, len(headers))
    set_widths(ws, widths)
    ws.auto_filter.ref = f"A1:{get_column_letter(len(headers))}{max(ws.max_row, 1)}"
    return ws


DETAIL_HEADERS = [
    "税号", "企业名称", "请求维度", "对账结论", "请求次数", "其中已计费次数", "其中免费期次数",
    "近12月开票金额", "发票为0", "近12月税务金额", "税务为0", "前12月总金额", "说明",
]
DETAIL_WIDTHS = [22, 32, 12, 14, 12, 16, 16, 16, 10, 16, 10, 16, 42]


def detail_row(r):
    return [
        r["credit"], r["company"], TYPE_LABEL[r["req"]], r["verdict"],
        r["queries"], r["charged"], r["free"],
        r["inv"], r["inv_zero"], r["tax"], r["tax_zero"], r["total"], r["note"],
    ]


def build():
    calls = load_bill()
    progress = load_progress()
    missing = [cc for cc in calls if cc not in progress]
    if missing:
        raise SystemExit(f"缺少重调结果 {len(missing)}")
    rows = [classify(calls[cc], progress[cc]) for cc in calls]
    rows.sort(key=lambda r: (ORDER.index(r["req"]), r["verdict"], r["credit"]))

    wb = Workbook()
    ws = wb.active
    ws.title = "汇总"

    def put(row, col, value, font=None, fill=None, align=None):
        cell = ws.cell(row, col, value)
        if font:
            cell.font = font
        if fill:
            cell.fill = fill
        cell.alignment = align or Alignment(vertical="center", wrap_text=True)
        return cell

    put(1, 1, "下游对账汇总（按税号）", TITLE_FONT)
    meta = [
        (2, "客户", "北京联信征信咨询有限责任公司"),
        (3, "区间（北京时间）", "2026-09-01 至 2026-10-31"),
        (4, "整理时间", datetime.now().strftime("%Y-%m-%d %H:%M:%S")),
        (5, "计数单位", "税号。同一个税号无论请求几次，只计 1 个。"),
        (6, "请求税号", "账单里出现过的税号，按该税号的请求维度（票 / 税 / 票税）归类。"),
        (7, "查得计费", "重调后查得，且前 12 个月总金额 > 0。金额窗口 202511–202611，含请求月。"),
        (8, "查得不计费", "重调后查得，但前 12 个月总金额 <= 0。不向下游计费。"),
        (9, "未查得", "重调结果为查无，或返回 002 且该维度没有实得数据。没有可计费的查得。"),
        (10, "重复查询", "同一税号的再次查询在原账单里是免费期，不增加请求税号，也不增加查得计费税号。"),
    ]
    for row, label, value in meta:
        put(row, 1, label, Font(bold=True), LABEL_FILL)
        put(row, 2, value)
        ws.merge_cells(start_row=row, start_column=2, end_row=row, end_column=6)

    header_row = 12
    headers = ["类型", "请求税号数", "查得计费税号数", "查得不计费税号数", "未查得税号数"]
    for col, h in enumerate(headers, 1):
        put(header_row, col, h, HEAD_FONT, HEAD_FILL, Alignment(horizontal="center", vertical="center", wrap_text=True))
    ws.row_dimensions[header_row].height = 30

    def bucket(req=None, verdict=None):
        return [r for r in rows if (req is None or r["req"] == req) and (verdict is None or r["verdict"] == verdict)]

    line = header_row + 1
    for req in ORDER:
        vals = [
            TYPE_LABEL[req],
            len(bucket(req)),
            len(bucket(req, "查得计费")),
            len(bucket(req, "查得不计费")),
            len(bucket(req, "未查得")),
        ]
        for col, v in enumerate(vals, 1):
            cell = put(line, col, v, align=Alignment(horizontal="center", vertical="center"))
            cell.border = THIN
            if col == 3:
                cell.font = Font(bold=True)
        line += 1
    totals = [
        "合计",
        len(rows),
        len(bucket(verdict="查得计费")),
        len(bucket(verdict="查得不计费")),
        len(bucket(verdict="未查得")),
    ]
    for col, v in enumerate(totals, 1):
        cell = put(line, col, v, Font(bold=True), TOTAL_FILL, Alignment(horizontal="center", vertical="center"))
        cell.border = THIN
    total_row = line

    line += 2
    put(line, 1, "票税户说明", SECTION_FONT, SECTION_FILL)
    ws.merge_cells(start_row=line, start_column=1, end_row=line, end_column=5)
    line += 1
    both = bucket("invoice+tax")
    if both:
        b = both[0]
        text = (
            f"{b['credit']} {b['company']}：请求票税 1 个税号。"
            f"发票近12月 {b['inv']}，税务近12月 {b['tax']}，总金额 {b['total']}。"
            f"结论：{b['verdict']}。{b['note']}。"
        )
    else:
        text = "无"
    put(line, 1, text)
    ws.merge_cells(start_row=line, start_column=1, end_row=line, end_column=6)
    ws.row_dimensions[line].height = 36

    line += 2
    put(line, 1, "原账单查询次数（不对账，仅对照）", SECTION_FONT, SECTION_FILL)
    ws.merge_cells(start_row=line, start_column=1, end_row=line, end_column=4)
    line += 1
    for col, h in enumerate(["类型", "查询次数", "对应税号数", "多出来的重复查询"], 1):
        put(line, col, h, HEAD_FONT, HEAD_FILL, Alignment(horizontal="center", vertical="center", wrap_text=True))
    line += 1
    q_total = n_total = 0
    for req in ORDER:
        subset = bucket(req)
        q = sum(r["queries"] for r in subset)
        n = len(subset)
        q_total += q
        n_total += n
        for col, v in enumerate([TYPE_LABEL[req], q, n, q - n], 1):
            cell = put(line, col, v, align=Alignment(horizontal="center"))
            cell.border = THIN
        line += 1
    for col, v in enumerate(["合计", q_total, n_total, q_total - n_total], 1):
        cell = put(line, col, v, Font(bold=True), TOTAL_FILL, Alignment(horizontal="center"))
        cell.border = THIN

    set_widths(ws, [22, 28, 22, 24, 18, 36])
    ws.row_dimensions[1].height = 24
    ws.sheet_view.showGridLines = False
    ws.auto_filter.ref = f"A{header_row}:E{total_row}"
    ws.freeze_panes = "A13"
    ws.page_setup.orientation = "landscape"
    ws.page_setup.fitToPage = True
    ws.page_setup.fitToWidth = 1
    ws.page_setup.fitToHeight = 1
    ws.sheet_properties.pageSetUpPr.fitToPage = True
    ws.print_title_rows = "1:1"
    ws.page_setup.paperSize = ws.PAPERSIZE_A4
    ws.sheet_view.view = "pageBreakPreview"

    groups = {
        "查得计费": [r for r in rows if r["verdict"] == "查得计费"],
        "查得不计费": [r for r in rows if r["verdict"] == "查得不计费"],
        "未查得": [r for r in rows if r["verdict"] == "未查得"],
    }
    for title, subset in groups.items():
        add_detail(wb, title, [detail_row(r) for r in subset], DETAIL_HEADERS, DETAIL_WIDTHS)
    add_detail(wb, "税号明细", [detail_row(r) for r in rows], DETAIL_HEADERS, DETAIL_WIDTHS)

    # print area of the summary is the reconciliation block, one page
    ws.print_area = "A1:F28"
    ws.page_setup.horizontalCentered = True
    ws.sheet_view.zoomScale = 120

    wb.save(OUT)
    print("xlsx", OUT)
    for req in ORDER:
        print(TYPE_LABEL[req], len(bucket(req)),
              "计费", len(bucket(req, "查得计费")),
              "不计费", len(bucket(req, "查得不计费")),
              "未查得", len(bucket(req, "未查得")),
              "queries", sum(r["queries"] for r in bucket(req)))


if __name__ == "__main__":
    build()
