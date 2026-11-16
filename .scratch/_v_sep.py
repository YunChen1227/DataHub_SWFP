import sys
sys.stdout.reconfigure(encoding="utf-8")
from openpyxl import load_workbook
P=r"c:\Users\XMMXYY\Documents\xwechat_files\wxid_t7vgbilml8m511_c2b2\msg\file\2026-11\冠辰科技-月度对账单-近12月金额核查.xlsx"
wb=load_workbook(P)
print("SHEETS:", wb.sheetnames)
print("\n== 对账单 ==")
ws=wb["对账单"]
for r in ws.iter_rows(values_only=True):
    if any(c not in (None,"") for c in r): print([c for c in r[1:4]])
print("\n== 近12月金额为0 rows:", wb["近12月金额为0"].max_row-1, "| 近12月核查明细 rows:", wb["近12月核查明细"].max_row-1)
ws=wb["近12月金额为0"]
for i,r in enumerate(ws.iter_rows(values_only=True)):
    if 1<=i<=5: print(" ",r[0], r[1], r[2], r[3], r[4], "总="+str(r[11]), "|",r[13])
