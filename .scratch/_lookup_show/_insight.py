# -*- coding: utf-8 -*-
import json
from decimal import Decimal
from collections import defaultdict
from pathlib import Path

base = Path(r"c:\workspace\DataHub_SWFP\.scratch\_lookup_show")
xy = json.loads((base / "inv_xyhzxxList.json").read_text(encoding="utf-8"))
kh = json.loads((base / "inv_khxsdqList.json").read_text(encoding="utf-8"))
sp = json.loads((base / "inv_spxxList.json").read_text(encoding="utf-8"))
kp = json.loads((base / "inv_kphzxxList.json").read_text(encoding="utf-8"))


def D(v):
    try:
        return Decimal(str(v or "0"))
    except Exception:
        return Decimal(0)


print("=== monthly buyer coverage ===")
by_m = defaultdict(list)
for r in xy:
    by_m[r["ssyf"]].append(r)
for m in sorted(by_m):
    rows = by_m[m]
    gfsl = rows[0].get("gfsl")
    amt = sum(D(r["ljkpjebhs"]) for r in rows)
    print(m, "returned", len(rows), "gfsl", gfsl, "amt", amt)

print("=== geo ===")
geo = defaultdict(lambda: Decimal(0))
for r in kh:
    geo[r.get("gfdjsxzqymc") or ""] += D(r.get("kpjebhs"))
total = sum(geo.values())
print("geo_total", total)
for name, amt in sorted(geo.items(), key=lambda x: -x[1]):
    share = (amt / total * 100) if total else 0
    print(f"{amt}\t{share:.1f}\t{name}")

print("=== red by buyer ===")
red = defaultdict(lambda: Decimal(0))
for r in xy:
    red[r["gfnsrmc"]] += D(r.get("hpjebhs"))
for name, amt in sorted(red.items(), key=lambda x: x[1]):
    if amt != 0:
        print(amt, name)

print("=== product by year ===")
yp = defaultdict(lambda: defaultdict(lambda: [Decimal(0), Decimal(0)]))
for r in sp:
    y = r["ssyf"][:4]
    yp[y][r["hwhlwmc"]][0] += D(r["spzje"])
    yp[y][r["hwhlwmc"]][1] += D(r["spzsl"])
for y in sorted(yp):
    for name, (amt, qty) in yp[y].items():
        print(y, name, amt, qty)

print("=== invoice rhythm ===")
for r in kp:
    print(r["ssyf"], "days", r["dykptsqb"], "blue_days", r["dykptslp"], "max", r["dzzgkpjejlp"], "cnt", r["ljkpcs"], "gap", r["dqlxwjyjlts"])

print("=== groups ===")
groups = {
    "天合系": ["天合光能", "天合储能"],
    "冠宇系": ["冠宇"],
    "力神系": ["力神"],
    "欣旺达": ["欣旺达"],
    "阿特斯": ["阿特斯"],
    "中化扬州": ["中化扬州"],
}
gamt = defaultdict(lambda: Decimal(0))
named = defaultdict(lambda: Decimal(0))
for r in xy:
    named[r["gfnsrmc"]] += D(r["ljkpjebhs"])
all_amt = sum(named.values())
print("buyer_amt", all_amt, "n", len(named))
ranked = sorted(named.items(), key=lambda x: -x[1])
c3 = sum(a for _, a in ranked[:3])
c5 = sum(a for _, a in ranked[:5])
print("c1", ranked[0][1], "c3", c3, "c5", c5)
for g, keys in groups.items():
    s = sum(a for n, a in named.items() if any(k in n for k in keys))
    print(g, s, f"{(s/all_amt*100):.1f}")
