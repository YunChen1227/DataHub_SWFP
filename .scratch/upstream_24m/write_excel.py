# -*- coding: utf-8 -*-
"""把 Go 重调结果(results.jsonl)按近24月口径写回各上游对账单（非破坏性副本）。

每个上游文件：加「24月重调明细」「24月金额为0」两个表。
金额=该类目(票/税)近24月金额；<=0 即不需向该上游缴费（高亮）。
查无/失败的不计入「金额为0」。results 里缺失的任务标为「未重调」。
"""
import json
import sys
from decimal import Decimal
from pathlib import Path

from openpyxl import load_workbook
from openpyxl.styles import Alignment, Font, PatternFill
from openpyxl.utils import get_column_letter

sys.stdout.reconfigure(encoding="utf-8")

BASE = Path(r"c:\workspace\DataHub_SWFP\.scratch\upstream_24m")
FILES = json.loads((BASE / "files.json").read_text(encoding="utf-8"))
ONLY = set(sys.argv[1:])  # 可选：只写指定 upstream（如 salesdata）

results = {}
rp = BASE / "results.jsonl"
if rp.exists():
    for line in rp.read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if line:
            r = json.loads(line)
            results[r["qkey"]] = r

UP_LABEL = {"entcredit": "证通", "salesdata": "凯盈云", "ctax": "惠众", "sy": "税友"}
CAT_LABEL = {"invoice": "票", "tax": "税"}

HEADERS = ["税号", "企业名称", "上游", "类目", "查得子源", "调用结果", "本次调用维度",
           f"近24月金额", "金额<=0", "近24月总金额", "总金额<=0", "说明",
           "窗口", "上游订单号", "重调耗时秒"]
WIDTHS = [22, 30, 10, 8, 20, 10, 12, 18, 10, 16, 10, 34, 14, 24, 12]


def money(v):
    if v in (None, ""):
        return ""
    return float(Decimal(str(v)))


def advice(outcome, cat, iszero):
    if outcome != "查得":
        return {"查无": "上游查无数据", "失败": "重调失败", "未重调": "尚未重调（见说明）"}.get(outcome, outcome)
    c = CAT_LABEL.get(cat, cat)
    return f"近24月{c}金额<=0，不需向该上游缴费" if iszero else f"近24月{c}金额>0，需缴费"


def build_sheet(wb, title, rows, note):
    if title in wb.sheetnames:
        del wb[title]
    ws = wb.create_sheet(title)
    hf = Font(bold=True, color="FFFFFF")
    hfill = PatternFill("solid", fgColor="1F4E79")
    zfill = PatternFill("solid", fgColor="FCE4D6")
    ws.append([note] + [""] * (len(HEADERS) - 1))
    ws.append(HEADERS)
    for c in ws[2]:
        c.font = hf
        c.fill = hfill
        c.alignment = Alignment(horizontal="center")
    for row, iszero, is_found_zero in rows:
        ws.append(row)
        if is_found_zero:
            for c in ws[ws.max_row]:
                c.fill = zfill
    for i, w in enumerate(WIDTHS, 1):
        ws.column_dimensions[get_column_letter(i)].width = w
    ws.freeze_panes = "A3"
    ws.auto_filter.ref = f"A2:{get_column_letter(len(HEADERS))}{ws.max_row}"


def main():
    missing_files = []
    for fkey, meta in FILES.items():
        up = meta["upstream"]
        if ONLY and up not in ONLY:
            continue
        frm, to = meta["from"], meta["to"]
        wkey = f"{frm}-{to}"
        rows_in = meta["rows"]
        if not rows_in:
            print(f"[{fkey}] 无查得行，跳过")
            continue
        detail, zero = [], []
        n_missing = 0
        for r in rows_in:
            qkey = f"{up}|{r['credit']}|{r['dim']}"
            res = results.get(qkey)
            if not res:
                outcome, amt, iszero, uid, elapsed, company = "未重调", "", None, "", "", ""
                n_missing += 1
            else:
                outcome = res["outcome"]
                uid = res.get("upUID", "")
                elapsed = res.get("elapsed", "")
                company = res.get("company", "")
                w = res.get("windows", {}).get(wkey)
                if outcome == "查得" and w:
                    amt, iszero = w["amount"], w["isZero"]
                else:
                    amt, iszero = "", None
            is_found_zero = (outcome == "查得" and iszero)
            got_dim = CAT_LABEL.get(r["dim"], r["dim"]) if outcome == "查得" else ""
            row = [
                r["credit"], company, UP_LABEL.get(up, up), CAT_LABEL.get(r["dim"], r["dim"]),
                r["sub"], outcome, got_dim,
                money(amt), ("是" if iszero else "否") if iszero is not None else "",
                money(amt), ("是" if iszero else "否") if iszero is not None else "",
                advice(outcome, r["dim"], iszero),
                wkey, uid, elapsed,
            ]
            detail.append((row, iszero, is_found_zero))
            if is_found_zero:
                zero.append((row, iszero, True))
        if n_missing:
            missing_files.append((fkey, n_missing, len(rows_in)))

        wb = load_workbook(meta["src"])
        note_all = (f"{UP_LABEL.get(up,up)}账单里「查得」的税号直连本上游重调，按近24个月口径"
                    f"（窗口{wkey}，账单月及之前24个月，含账单月，北京时间）判定该类目金额。"
                    f"金额<=0 的行见「24月金额为0」并高亮。")
        note_zero = f"以下税号近24月{UP_LABEL.get(up,up)}金额<=0（窗口{wkey}），不需向该上游缴费。查无/失败不在此表。"
        build_sheet(wb, "24月重调明细", detail, note_all)
        build_sheet(wb, "24月金额为0", zero, note_zero)
        wb.save(meta["out"])
        print(f"[{fkey}] 写出 {Path(meta['out']).name}: 明细{len(detail)} 金额为0 {len(zero)} 未重调 {n_missing}")

    if missing_files:
        print("\n⚠ 以下文件仍有未重调的任务（results.jsonl 缺对应上游结果）：")
        for fkey, nm, tot in missing_files:
            print(f"  {fkey}: {nm}/{tot} 未重调")


if __name__ == "__main__":
    main()
