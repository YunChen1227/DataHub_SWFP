# -*- coding: utf-8 -*-
import json
from decimal import Decimal
from collections import defaultdict
from pathlib import Path

raw = Path(r"c:\workspace\DataHub_SWFP\.scratch\_lookup_913205007890695060.json").read_text(encoding="utf-8")
obj = json.loads(raw)
rng = obj["body"]["result"]["range"]
if isinstance(rng, str):
    rng = json.loads(rng)
inv = rng["发票数据聚合"]
kp = inv["kphzxxList"]
xy = inv["xyhzxxList"]
sy = inv["syhzxxList"]


def D(v):
    try:
        return Decimal(str(v or "0"))
    except Exception:
        return Decimal(0)


print("=== months ===")
for r in kp:
    print(r["ssyf"], "kpje", r["kpje"], "se", r["ljse"], "cnt", r["ljkpcs"], "hpje", r["hpje"], "fpje", r.get("fpje"), "days", r["dykptsqb"], "max", r["dzzgkpjejlp"], "gap", r["dqlxwjyjlts"])

print("=== buyer coverage ===")
by = defaultdict(list)
for r in xy:
    by[r["ssyf"]].append(r)
for m in sorted(by):
    rows = by[m]
    print(m, "n", len(rows), "gfsl", rows[0].get("gfsl"), "amt", sum(D(x["ljkpjebhs"]) for x in rows))

print("=== seller coverage ===")
by = defaultdict(list)
for r in sy:
    by[r["ssyf"]].append(r)
for m in sorted(by):
    rows = by[m]
    print(m, "n", len(rows), "xfsl", rows[0].get("xfsl"), "amt", sum(D(x["ljkpjebhs"]) for x in rows))

print("=== red buyers ===")
red = defaultdict(lambda: Decimal(0))
net = defaultdict(lambda: Decimal(0))
for r in xy:
    red[r["gfnsrmc"]] += D(r.get("hpjebhs"))
    net[r["gfnsrmc"]] += D(r.get("ljkpjebhs"))
for name, amt in sorted(red.items(), key=lambda x: x[1]):
    if amt != 0:
        print(amt, "net", net[name], name)

print("=== goods detail ===")
for r in inv["spxxList"]:
    print(r.get("ssyf"), r.get("hwhlwmc"), r.get("spzje"), r.get("spzsl"), r.get("sl"), r.get("hwhlwbm"))
