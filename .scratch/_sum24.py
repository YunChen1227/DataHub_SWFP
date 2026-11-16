# -*- coding: utf-8 -*-
import json, sys
from collections import Counter
sys.stdout.reconfigure(encoding="utf-8")

recs = {}
for line in open("lianxin_24m/progress.jsonl", encoding="utf-8"):
    line = line.strip()
    if line:
        r = json.loads(line)
        recs[r["key"]] = r
rs = list(recs.values())
print("total tasks:", len(rs))
print("outcome:", dict(Counter(r["outcome"] for r in rs)))
print("bodyCode:", dict(Counter(r.get("bodyCode") for r in rs)))

fails = [r for r in rs if r["outcome"] == "失败"]
print("\n失败 count:", len(fails))
for r in fails[:20]:
    print("  ", r["credit"], r["dataType"], r.get("errorCode"), r.get("errorMsg"))

zero = [r for r in rs if r["outcome"] == "查得" and r.get("isZero")]
print("\n查得 且 近24月总金额<=0:", len(zero))
for r in sorted(zero, key=lambda x: x["dataType"]):
    print(f'  {r["credit"]}  {r["dataType"]:7} total={r.get("total")}  窗口{r.get("frm")}-{r.get("to")}  {r.get("company") or ""}')
