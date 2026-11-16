# -*- coding: utf-8 -*-
import sys
sys.stdout.reconfigure(encoding="utf-8")
from openpyxl import load_workbook

files = [
    r"c:\Users\XMMXYY\Downloads\下游账单-北京联信征信咨询有限责任公司-2026-09-01至2026-09-30.xlsx",
    r"c:\Users\XMMXYY\Downloads\下游账单-北京联信征信咨询有限责任公司-2026-10-01至2026-10-31.xlsx",
]
for p in files:
    print("#" * 80)
    print("FILE", p.split("\\")[-1])
    wb = load_workbook(p, read_only=True, data_only=True)
    print("SHEETS", wb.sheetnames)
    for title in wb.sheetnames:
        ws = wb[title]
        print("=" * 60, "\nSHEET:", title)
        n = 0
        for row in ws.iter_rows(values_only=True):
            if row is None or all(c is None or str(c).strip() == "" for c in row):
                continue
            n += 1
            if n <= 8:
                print(n, [ (c if not isinstance(c, str) else c[:30]) for c in row])
        print("... non-empty rows:", n)
