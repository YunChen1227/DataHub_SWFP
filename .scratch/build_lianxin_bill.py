# -*- coding: utf-8 -*-
"""整理「我该给北京联信发的账单」，按三个接口维度统计：只调用票 / 只调用税 / 同时调用票税。
分类依据 = 请求维度（客户实际调用的接口）；票税作为独立第三类，不拆进票或税。
9月 + 10月 放在同一个文件里。

口径：原账单「查得」的税号，经近24个月重调判定——
  - 计费（应收）= 调用结果=查得 且 近24月总金额 > 0（总金额按该次实得维度的合计）。
  - 不计费 = 近24月总金额<=0（零数据规则，不能向下游收费），已剔除。
  - 待确认 = 重调查无/未取得，拿不到数据无法判定，单列不计入账单。
"""
import sys
from pathlib import Path
from decimal import Decimal

from openpyxl import Workbook, load_workbook
from openpyxl.styles import Alignment, Font, PatternFill
from openpyxl.utils import get_column_letter

sys.stdout.reconfigure(encoding="utf-8")

DL = Path(r"c:\Users\XMMXYY\Downloads")
SRC = {
    "9月": DL / "下游账单-北京联信征信咨询有限责任公司-2026-09-01至2026-09-30-24月零金额.xlsx",
    "10月": DL / "下游账单-北京联信征信咨询有限责任公司-2026-10-01至2026-10-31-24月零金额.xlsx",
}
OUT = DL / "北京联信-应收账单-2026年9-10月.xlsx"
SHEET = "24月重调明细"

# 请求维度 -> (键, 中文接口名, 明细表短名)
DIMS = [
    ("invoice", "只调用票", "只票"),
    ("tax", "只调用税", "只税"),
    ("invoice+tax", "同时调用票税", "票税"),
]
DIM_CN = {k: cn for k, cn, _ in DIMS}


def num(v):
    if v in (None, ""):
        return None
    try:
        return Decimal(str(v))
    except Exception:
        return None


def fnum(v):
    a = num(v)
    return float(a) if a is not None else ""


def read_month(path):
    wb = load_workbook(path, read_only=True, data_only=True)
    ws = wb[SHEET]
    rows = list(ws.iter_rows(values_only=True))
    wb.close()
    ix = {h: i for i, h in enumerate(rows[1])}
    bill = {k: [] for k, _, _ in DIMS}   # 计费明细
    zero = {k: 0 for k, _, _ in DIMS}    # 不计费(金额为0)
    pend = []                            # 待确认
    for r in rows[2:]:
        cc = str(r[ix["税号"]] or "").strip()
        if not cc:
            continue
        name = str(r[ix["企业名称"]] or "").strip()
        reqdim = str(r[ix["请求维度"]] or "").strip()
        result = str(r[ix["调用结果"]] or "").strip()
        iszero = str(r[ix["总金额<=0"]] or "").strip()
        rec = (cc, name, r[ix["近24月开票金额"]], r[ix["近24月税务金额"]], r[ix["近24月总金额"]])
        if result != "查得":
            pend.append((cc, name, reqdim, result))
            continue
        if reqdim not in bill:
            reqdim = "invoice+tax" if "+" in reqdim else reqdim
        if iszero == "是":
            zero[reqdim] = zero.get(reqdim, 0) + 1
        else:
            bill[reqdim].append(rec)
    return bill, zero, pend


HEADER_FONT = Font(bold=True, color="FFFFFF")
HEADER_FILL = PatternFill("solid", fgColor="1F4E79")


def style_header(ws, rownum):
    for cell in ws[rownum]:
        if cell.value is None:
            continue
        cell.font = HEADER_FONT
        cell.fill = HEADER_FILL
        cell.alignment = Alignment(horizontal="center")


def detail_sheet(wb, title, cn, records):
    ws = wb.create_sheet(title)
    hdr = ["序号", "税号", "企业名称", "近24月开票金额", "近24月税务金额", "近24月总金额"]
    ws.append([f"{cn}  {title.split('-')[0]}  共 {len(records)} 条"] + [""] * (len(hdr) - 1))
    ws["A1"].font = Font(bold=True, size=12)
    ws.append(hdr)
    style_header(ws, 2)
    for i, (cc, name, inv, tax, tot) in enumerate(sorted(records, key=lambda x: x[0]), 1):
        ws.append([i, cc, name, fnum(inv), fnum(tax), fnum(tot)])
    for col, w in enumerate([6, 22, 34, 18, 18, 16], 1):
        ws.column_dimensions[get_column_letter(col)].width = w
    ws.freeze_panes = "A3"
    ws.auto_filter.ref = f"A2:{get_column_letter(len(hdr))}{ws.max_row}"


def main():
    data = {m: read_month(p) for m, p in SRC.items()}

    wb = Workbook()
    ws = wb.active
    ws.title = "账单汇总"
    ws.append(["北京联信征信咨询有限责任公司 — 应收账单（2026年9月 / 10月）"])
    ws["A1"].font = Font(bold=True, size=13)
    ws.append([])
    ws.append(["按三个接口维度统计：只调用票 / 只调用税 / 同时调用票税（分类依据=请求维度，票税为独立类，不拆分）。"])
    ws.append(["计费=原「查得」且近24月总金额>0；近24月总金额<=0 按零数据规则不计费（已剔除）；重调查无/未取得单列「待确认」不计入。"])
    ws.append([])
    ws.append(["月份", "接口维度", "计费条数(税号)", "不计费(金额为0)", "待确认(查无/未取得)"])
    style_header(ws, 6)

    grand = {k: 0 for k, _, _ in DIMS}
    for m in SRC:
        bill, zero, pend = data[m]
        for k, cn, _ in DIMS:
            ws.append([m, cn, len(bill[k]), zero.get(k, 0), ""])
            grand[k] += len(bill[k])
        bill_sum = sum(len(bill[k]) for k, _, _ in DIMS)
        zero_sum = sum(zero.values())
        row = ["", "本月合计", bill_sum, zero_sum, len(pend)]
        ws.append(row)
        for cell in ws[ws.max_row]:
            if cell.value not in (None, ""):
                cell.font = Font(bold=True)
    ws.append([])
    total_bill = sum(grand.values())
    ws.append(["两月合计", "", total_bill, "", ""])
    ws[ws.max_row][0].font = Font(bold=True)
    ws.append(["", "只调用票", grand["invoice"], "", ""])
    ws.append(["", "只调用税", grand["tax"], "", ""])
    ws.append(["", "同时调用票税", grand["invoice+tax"], "", ""])
    for col, w in enumerate([10, 16, 16, 16, 20], 1):
        ws.column_dimensions[get_column_letter(col)].width = w

    for m in SRC:
        bill, zero, pend = data[m]
        for k, cn, short in DIMS:
            if bill[k]:
                detail_sheet(wb, f"{m}-{short}", cn, bill[k])

    wsp = wb.create_sheet("待确认(未计费)")
    hdr = ["月份", "税号", "企业名称", "接口维度", "重调结果"]
    wsp.append(["重调查无/未取得，无法确认近24月金额，暂不计入账单，待人工核对"])
    wsp.append(hdr)
    style_header(wsp, 2)
    for m in SRC:
        for cc, name, reqdim, result in data[m][2]:
            wsp.append([m, cc, name, DIM_CN.get(reqdim, reqdim), result])
    for col, w in enumerate([8, 22, 34, 14, 12], 1):
        wsp.column_dimensions[get_column_letter(col)].width = w
    wsp.freeze_panes = "A3"

    wb.save(OUT)
    print("写出:", OUT)
    for m in SRC:
        bill, zero, pend = data[m]
        print(f"[{m}] " + " / ".join(f"{cn} {len(bill[k])}" for k, cn, _ in DIMS)
              + f" | 不计费 {sum(zero.values())} | 待确认 {len(pend)}")
    print("两月合计: " + " / ".join(f"{cn} {grand[k]}" for k, cn, _ in DIMS)
          + f" | 总计费 {total_bill}")


if __name__ == "__main__":
    main()
