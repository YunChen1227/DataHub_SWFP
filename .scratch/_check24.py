# -*- coding: utf-8 -*-
import importlib.util, sys
sys.stdout.reconfigure(encoding="utf-8")
spec = importlib.util.spec_from_file_location("m", "recall_lianxin_24m.py")
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)
from collections import Counter

allkeys = set()
for name, cfg in m.FILES.items():
    frm, to = m.window_for(cfg["anchor"])
    es = m.read_queried(cfg["src"])
    print(name, "entries", len(es), "window", frm, to)
    print("   got:", dict(Counter(e["got"] for e in es)))
    print("   req:", dict(Counter(e["req"] for e in es)))
    for e in es:
        dt = m.dim_to_type(e["got"], e["req"])
        allkeys.add(f"{dt}_{e['credit']}_{frm}-{to}")
print("total unique (credit,dt,window) tasks:", len(allkeys))
