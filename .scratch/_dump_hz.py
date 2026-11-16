import sys
sys.stdout.reconfigure(encoding="utf-8")
from openpyxl import load_workbook
p=r"c:\Users\XMMXYY\Documents\xwechat_files\wxid_t7vgbilml8m511_c2b2\msg\file\2026-11\冠辰科技-月度对账单(1).xlsx"
wb=load_workbook(p, read_only=True, data_only=True)
for s in wb.worksheets:
    ws=wb[s.title]
    print("="*70)
    print("SHEET:", s.title)
    n=0
    for row in ws.iter_rows(values_only=True):
        if row is None or all(c is None or str(c).strip()=="" for c in row):
            continue
        n+=1
        if n<=12:
            print(n, [ (c if not isinstance(c,str) else c[:30]) for c in row])
    print("... non-empty rows:", n)
