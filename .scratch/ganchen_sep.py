# -*- coding: utf-8 -*-
"""冠辰科技(惠众)9月对账单 近12月金额核查。"""
from collections import defaultdict
from pathlib import Path

from openpyxl import load_workbook

import swfp_recon as E

SRC = Path(r"c:\Users\XMMXYY\Documents\xwechat_files\wxid_t7vgbilml8m511_c2b2\msg\file\2026-11\冠辰科技-月度对账单.xlsx")
OUT = SRC.with_name("冠辰科技-月度对账单-近12月金额核查.xlsx")
WORK = Path(r"c:\workspace\DataHub_SWFP\.scratch\ganchen_sep_recall")


def load_tasks(cfg):
    wb = load_workbook(cfg.src, read_only=True, data_only=True)
    dims = defaultdict(lambda: {"inv": False, "tax": False})
    name = {}
    ws = wb["9月明细"]
    it = ws.iter_rows(values_only=True)
    hdr = list(next(it))
    ix = {c: i for i, c in enumerate(hdr)}
    for r in it:
        if not r or not r[ix["税号"]]:
            continue
        t = E.norm(r[ix["税号"]])
        if E.truthy(r[ix["计费票"]]):
            dims[t]["inv"] = True
        if E.truthy(r[ix["计费税"]]):
            dims[t]["tax"] = True
        if "企业名称" in ix and r[ix["企业名称"]]:
            name[t] = r[ix["企业名称"]]
    tasks = []
    for t, d in dims.items():
        dt = "both" if d["inv"] and d["tax"] else ("invoice" if d["inv"] else "tax")
        req = {"both": "票税", "invoice": "票", "tax": "税"}[dt]
        tasks.append({"credit": t, "dataType": dt, "request": req,
                      "name": name.get(t, ""), "block": "9月明细"})
    tasks.sort(key=lambda x: x["credit"])
    return tasks


if __name__ == "__main__":
    cfg = E.Config(src=SRC, out=OUT, work=WORK,
                   win_from=202509, win_to=202609, window_label="2025-09 至 2026-09",
                   summary_start=9, load_tasks=load_tasks)
    E.run(cfg)
