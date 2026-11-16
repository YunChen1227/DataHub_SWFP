# -*- coding: utf-8 -*-
"""从 4 个上游对账单的「数据库无记录（待人工）」表抽取查得税号，生成 Go 重调任务。

每行 = 一个 (上游, 税号, 类目票/税)。查询一次上游即可（窗口在本地按账单月套用），
故按 (upstream, credit, dim) 去重成查询任务，每个任务带它需要的窗口列表与来源文件行。
"""
import json
import sys
from pathlib import Path

from openpyxl import load_workbook

sys.stdout.reconfigure(encoding="utf-8")

DL = Path(r"c:\Users\XMMXYY\Downloads")
OUT = Path(r"c:\workspace\DataHub_SWFP\.scratch\upstream_24m")
SHEET = "数据库无记录（待人工）"
MONTHS_BACK = 24

# upstream -> 文件名模板里的中文名
FILES = [
    # (upstream, 文件中文名, month_key, anchor_yyyymm, 区间串)
    ("entcredit", "证通", "sep", 202609, "2026-09-01至2026-09-30"),
    ("entcredit", "证通", "oct", 202610, "2026-10-01至2026-10-31"),
    ("salesdata", "凯盈云", "sep", 202609, "2026-09-01至2026-09-30"),
    ("salesdata", "凯盈云", "oct", 202610, "2026-10-01至2026-10-31"),
    ("ctax", "惠众", "sep", 202609, "2026-09-01至2026-09-30"),
    ("ctax", "惠众", "oct", 202610, "2026-10-01至2026-10-31"),
    ("sy", "税友", "sep", 202609, "2026-09-01至2026-09-30"),
    ("sy", "税友", "oct", 202610, "2026-10-01至2026-10-31"),
]

DIM_OF = {"票": "invoice", "税": "tax"}


def window_for(anchor):
    y, m = anchor // 100, anchor % 100
    sm, sy = m - MONTHS_BACK, y
    while sm <= 0:
        sm += 12
        sy -= 1
    return sy * 100 + sm, anchor


def read_rows(path):
    wb = load_workbook(path, read_only=True, data_only=True)
    if SHEET not in wb.sheetnames:
        wb.close()
        return []
    ws = wb[SHEET]
    it = ws.iter_rows(values_only=True)
    header = next(it)
    idx = {h: i for i, h in enumerate(header)}
    out = []
    for row in it:
        if not row or len(row) < len(header) or not row[idx["税号"]]:
            continue
        if str(row[idx["税号"]]).strip().startswith("(无数据"):
            continue
        if str(row[idx["查得情况"]]).strip() != "查得":
            continue
        cat = str(row[idx["类目"]] or "").strip()
        if cat not in DIM_OF:
            raise SystemExit(f"未知类目 {cat!r} @ {path.name}")
        out.append({
            "credit": str(row[idx["税号"]]).strip(),
            "cat": cat,
            "dim": DIM_OF[cat],
            "sub": str(row[idx.get("查得子源", 16)] or "").strip(),
        })
    wb.close()
    return out


def main():
    OUT.mkdir(parents=True, exist_ok=True)
    files_meta = {}          # file_key -> meta + rows
    tasks = {}               # qkey -> task
    for upstream, cn, mk, anchor, span in FILES:
        fname = f"下游"  # placeholder, not used
        path = DL / f"上游对账-{cn}-{span}.xlsx"
        frm, to = window_for(anchor)
        rows = read_rows(path)
        file_key = f"{upstream}_{mk}"
        files_meta[file_key] = {
            "upstream": upstream, "cn": cn, "month": mk, "anchor": anchor,
            "span": span, "src": str(path),
            "out": str(DL / f"上游对账-{cn}-{span}-24月零金额.xlsx"),
            "from": frm, "to": to, "rows": rows,
        }
        for r in rows:
            qkey = f"{upstream}|{r['credit']}|{r['dim']}"
            t = tasks.get(qkey)
            if not t:
                t = {"qkey": qkey, "upstream": upstream, "credit": r["credit"],
                     "dim": r["dim"], "windows": [], "subs": set()}
                tasks[qkey] = t
            if [frm, to] not in t["windows"]:
                t["windows"].append([frm, to])
            t["subs"].add(r["sub"])
        print(f"[{file_key}] 查得行 {len(rows)} 窗口 {frm}-{to}")

    for t in tasks.values():
        t["subs"] = sorted(t["subs"])
    tasks_list = list(tasks.values())

    # 窗口数统计
    total_upstream_calls = len(tasks_list)
    total_judgements = sum(len(t["windows"]) for t in tasks_list)
    by_up = {}
    for t in tasks_list:
        by_up[t["upstream"]] = by_up.get(t["upstream"], 0) + 1
    print(f"\n唯一上游查询任务 {total_upstream_calls} 个（判定 {total_judgements} 条）")
    print("按上游:", by_up)

    (OUT / "tasks.json").write_text(
        json.dumps(tasks_list, ensure_ascii=False, indent=0), encoding="utf-8")
    # files_meta rows 可能很大，但只有待人工行，无妨
    (OUT / "files.json").write_text(
        json.dumps(files_meta, ensure_ascii=False), encoding="utf-8")
    print("写出 tasks.json / files.json ->", OUT)


if __name__ == "__main__":
    main()
