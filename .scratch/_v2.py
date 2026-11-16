import sys
sys.stdout.reconfigure(encoding="utf-8")
from openpyxl import load_workbook
P=r"c:\Users\XMMXYY\Documents\xwechat_files\wxid_t7vgbilml8m511_c2b2\msg\file\2026-11\冠辰科技-月度对账单(1)-近12月金额核查.xlsx"
ws=load_workbook(P)["近12月金额为0"]
for i,r in enumerate(ws.iter_rows(values_only=True)):
    if 1<=i<=5: print(r[0], "|", r[1], "|", r[2], "|总金额", r[12])
