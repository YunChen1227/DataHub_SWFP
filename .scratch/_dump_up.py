# -*- coding: utf-8 -*-
import sys, glob
sys.stdout.reconfigure(encoding="utf-8")
from openpyxl import load_workbook

files = sorted(glob.glob(r"c:\Users\XMMXYY\Downloads\上游对账-*2026-*.xlsx"))
for p in files:
    print("#" * 80)
    print("FILE:", p.split("\\")[-1])
    wb = load_workbook(p, read_only=True, data_only=True)
    print("SHEETS:", wb.sheetnames)
    for title in wb.sheetnames:
        ws = wb[title]
        print("=" * 50, "SHEET:", title)
        n = 0
        for row in ws.iter_rows(values_only=True):
            if row is None or all(c is None or str(c).strip() == "" for c in row):
                continue
            n += 1
            if n <= 5:
                print(" ", n, [(c if not isinstance(c, str) else c[:26]) for c in row])
        print("  ... non-empty rows:", n)
    wb.close()
