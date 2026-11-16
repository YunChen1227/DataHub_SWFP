# -*- coding: utf-8 -*-
import json
from decimal import Decimal
from collections import defaultdict
from pathlib import Path

base = Path(r"c:\workspace\DataHub_SWFP\.scratch\_lookup_show")
xy = json.loads((base / "inv_xyhzxxList.json").read_text(encoding="utf-8"))
sy = json.loads((base / "inv_syhzxxList.json").read_text(encoding="utf-8"))


def D(v):
    try:
        return Decimal(str(v or "0"))
    except Exception:
        return Decimal(0)


def agg(rows, name_k, tax_k, amt_k, cnt_k):
    m = defaultdict(lambda: {"amt": Decimal(0), "cnt": 0, "months": set(), "taxno": ""})
    for r in rows:
        name = (r.get(name_k) or "").strip()
        rec = m[name]
        rec["amt"] += D(r.get(amt_k))
        rec["cnt"] += int(D(r.get(cnt_k)))
        rec["months"].add(r.get("ssyf") or "")
        rec["taxno"] = r.get(tax_k) or rec["taxno"]
    return m


down = agg(xy, "gfnsrmc", "gfnsrsbh", "ljkpjebhs", "ljkpcs")
up = agg(sy, "xfmc", "xfnsrsbh", "ljkpjebhs", "ljkpcs")
print("DOWN", len(down), "rows", len(xy))
for name, rec in sorted(down.items(), key=lambda x: -x[1]["amt"]):
    ms = sorted(x for x in rec["months"] if x)
    span = ms[0] + ".." + ms[-1]
    print("\t".join([str(rec["amt"]), str(rec["cnt"]), str(len(ms)), span, rec["taxno"], name]))
print("UP", len(up), "rows", len(sy))
for name, rec in sorted(up.items(), key=lambda x: -x[1]["amt"]):
    ms = sorted(x for x in rec["months"] if x)
    span = ms[0] + ".." + ms[-1]
    print("\t".join([str(rec["amt"]), str(rec["cnt"]), str(len(ms)), span, rec["taxno"], name]))
