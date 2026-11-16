import sys
sys.stdout.reconfigure(encoding="utf-8")
from openpyxl import load_workbook
from collections import defaultdict
P=r"c:\Users\XMMXYY\Documents\xwechat_files\wxid_t7vgbilml8m511_c2b2\msg\file\2026-11\冠辰科技-月度对账单(1).xlsx"
wb=load_workbook(P, read_only=True, data_only=True)
def norm(t): return str(t).strip().upper()
dims=defaultdict(lambda:{"inv":False,"tax":False}); name={}; blocks=defaultdict(set); rows=defaultdict(int)
for sh,blk in (("10月明细A","A"),("10月明细C","C")):
    ws=wb[sh]; it=ws.iter_rows(values_only=True); hdr=list(next(it)); ix={c:i for i,c in enumerate(hdr)}
    for r in it:
        if not r or not r[ix["税号"]]: continue
        t=norm(r[ix["税号"]])
        if r[ix["计费票"]] in (1,"1",1.0): dims[t]["inv"]=True
        if r[ix["计费税"]] in (1,"1",1.0): dims[t]["tax"]=True
        nm=r[ix.get("企业名称")] if "企业名称" in ix else None
        if nm: name[t]=nm
        blocks[t].add(blk); rows[t]+=1
tot=len(dims)
both=sum(1 for t in dims if dims[t]["inv"] and dims[t]["tax"])
onlyi=sum(1 for t in dims if dims[t]["inv"] and not dims[t]["tax"])
onlyt=sum(1 for t in dims if dims[t]["tax"] and not dims[t]["inv"])
none=sum(1 for t in dims if not dims[t]["inv"] and not dims[t]["tax"])
inboth=sum(1 for t in dims if blocks[t]=={"A","C"})
print("unique tax:",tot,"| both(票税):",both,"only invoice(票):",onlyi,"only tax(税):",onlyt,"neither:",none)
print("appear in both A&C blocks:",inboth)
print("sum billed-dim count =",both*2+onlyi+onlyt)
