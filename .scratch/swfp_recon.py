# -*- coding: utf-8 -*-
"""惠众对账单近12月金额核查引擎（参数化，按月复用）。

对账单里每个计费税号，按其计费维度(票/税/票税)走税务发票聚合查询接口，
判定近12个月总金额是否为0，并标注查得/未查得。窗口按账单月锚定 [账单月-12, 账单月]，
与系统 zerorule v1 一致。
"""
import hashlib
import json
import threading
import time
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass, field
from decimal import Decimal, InvalidOperation
from pathlib import Path
from typing import Callable, List

from openpyxl import load_workbook
from openpyxl.styles import Alignment, Border, Font, PatternFill, Side
from openpyxl.utils import get_column_letter

APP_KEY = "j92eme33uipj"
SECRET = "4c08fd3cafb7615e99506ae3e5d694f4"
BASE = "http://www.aiszcloud.cn:8081"
PATH = "/v1/openapi/zlx/querySrmxSWFP"
WORKERS = 6
TIMEOUT = 40
TYPE_LABEL = {"invoice": "票", "tax": "税", "both": "票税"}
_LOCK = threading.Lock()


@dataclass
class Config:
    src: Path
    out: Path
    work: Path
    win_from: int
    win_to: int
    window_label: str
    summary_start: int
    load_tasks: Callable[["Config"], List[dict]]

    @property
    def raw(self):
        return self.work / "raw"

    @property
    def progress(self):
        return self.work / "progress.jsonl"


# ---------- helpers ----------
def norm(t):
    return str(t).strip().upper()


def truthy(v):
    return v in (1, "1", 1.0, True)


def sign(body):
    parts = [f"{k}{body[k]}" for k in sorted(body) if body[k] not in (None, "")]
    return hashlib.md5(("".join(parts) + SECRET).encode("utf-8")).hexdigest()


def year_month(v):
    if v is None or isinstance(v, bool):
        return 0
    s = str(v).strip()
    digits = []
    for c in s:
        if len(digits) >= 6:
            break
        if c.isdigit():
            digits.append(c)
        elif c in "-/.":
            continue
        else:
            return 0
    if len(digits) < 6:
        return 0
    ym = int("".join(digits))
    if ym % 100 < 1 or ym % 100 > 12:
        return 0
    return ym


def parse_number(v):
    if v is None or isinstance(v, bool):
        return None
    if isinstance(v, (int, Decimal)):
        return Decimal(v)
    if isinstance(v, float):
        return Decimal(str(v))
    s = str(v).strip().replace(",", "")
    if not s or s.lower() == "null":
        return None
    try:
        return Decimal(s)
    except InvalidOperation:
        return None


def sum_list(items, amount_keys, month_keys, frm, to):
    totals = [Decimal(0) for _ in amount_keys]
    if not isinstance(items, list):
        return totals
    for it in items:
        if not isinstance(it, dict):
            continue
        ym = 0
        for k in month_keys:
            ym = year_month(it.get(k))
            if ym:
                break
        if ym == 0 or ym < frm or ym > to:
            continue
        for i, k in enumerate(amount_keys):
            n = parse_number(it.get(k))
            if n is not None:
                totals[i] += n
    return totals


def company_name(inv, tax):
    for block in (inv, tax):
        if isinstance(block, dict):
            info = block.get("nsrjbxx") or {}
            if isinstance(info, dict):
                nm = str(info.get("nsrmc") or "").strip()
                if nm:
                    return nm
    return ""


def judge(rng, dt, frm, to):
    inv = (rng or {}).get("发票数据聚合") or {}
    tax = (rng or {}).get("税务数据聚合") or {}
    scope = (rng or {}).get("dataScope") or {}
    (inv_amt,) = sum_list(inv.get("kphzxxList"), ["kpje"], ["ssyf", "kpqj"], frm, to)
    ysx, ynse = sum_list(tax.get("sbsjList"), ["ysxssr", "ynse"], ["sssjz", "sssjq", "sbrq"], frm, to)
    (sjje,) = sum_list(tax.get("zsbxxList"), ["sjje"], ["sssjz", "sssjq", "rkrq"], frm, to)
    tax_parts = [ysx, ynse, sjje]
    tax_amt = sum((p for p in tax_parts if p > 0), Decimal(0))
    want_inv, want_tax = dt in ("invoice", "both"), dt in ("tax", "both")
    total = Decimal(0)
    if want_inv:
        total += inv_amt
    if want_tax:
        total += tax_amt
    return {
        "company": company_name(inv, tax),
        "got_invoice": bool(scope.get("发票")), "got_tax": bool(scope.get("税务")),
        "inv_amt": inv_amt if want_inv else None, "tax_amt": tax_amt if want_tax else None,
        "tax_parts": [ysx, ynse, sjje] if want_tax else None,
        "inv_zero": (inv_amt <= 0) if want_inv else None,
        "tax_zero": all(p <= 0 for p in tax_parts) if want_tax else None,
        "total": total, "is_zero": total <= 0,
    }


def query(credit, dt):
    body = {"creditCode": credit, "dataType": dt}
    payload = {"encryptionType": 1, "appKey": APP_KEY, "sign": sign(body), "body": body}
    data = json.dumps(payload, ensure_ascii=False).encode("utf-8")
    req = urllib.request.Request(BASE + PATH, data=data,
        headers={"Content-Type": "application/json; charset=utf-8"}, method="POST")
    t0 = time.perf_counter()
    try:
        with urllib.request.urlopen(req, timeout=TIMEOUT) as resp:
            return resp.status, resp.read().decode("utf-8", "replace"), time.perf_counter() - t0, ""
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace"), time.perf_counter() - t0, ""
    except Exception as e:
        return 0, "", time.perf_counter() - t0, f"{type(e).__name__}: {e}"


def obtained(rec):
    dt = rec.get("dataType")
    gi, gt = rec.get("gotInvoice"), rec.get("gotTax")
    if dt == "invoice":
        return bool(gi)
    if dt == "tax":
        return bool(gt)
    return bool(gi) or bool(gt)


def interpret(credit, dt, status, raw, elapsed, err, frm, to):
    rec = {"credit": credit, "dataType": dt, "httpStatus": status, "elapsed": round(elapsed, 2),
           "error": err, "errorCode": "", "errorMsg": "", "bodyCode": "", "bodyMsg": "", "reqid": "",
           "company": "", "gotInvoice": None, "gotTax": None, "invAmt": None, "taxAmt": None,
           "ysxssr": None, "ynse": None, "sjje": None, "invZero": None, "taxZero": None,
           "total": None, "isZero": None, "outcome": "失败"}
    if err or not raw:
        rec["errorMsg"] = err or "empty body"
        return rec
    try:
        obj = json.loads(raw)
    except json.JSONDecodeError:
        rec["errorMsg"] = raw[:300]
        return rec
    head, body = obj.get("head") or {}, obj.get("body") or {}
    rec["errorCode"] = str(head.get("errorCode") or "")
    rec["errorMsg"] = str(head.get("errorMsg") or "")
    rec["bodyCode"] = str(body.get("code") or "")
    rec["bodyMsg"] = str(body.get("msg") or "")
    rec["reqid"] = str(body.get("reqid") or "")
    if rec["errorCode"] != "0":
        return rec
    if rec["bodyCode"] not in {"001", "002"}:
        rec["outcome"] = "查无" if rec["bodyCode"] in {"999", ""} else "失败"
        return rec
    result = body.get("result") or {}
    rng = result.get("range")
    if isinstance(rng, str):
        try:
            rng = json.loads(rng)
        except json.JSONDecodeError:
            rec["errorMsg"] = "range 非法 JSON"
            return rec
    if not isinstance(rng, dict):
        rec["outcome"] = "查无"
        return rec
    j = judge(rng, dt, frm, to)
    rec["company"] = j["company"]
    rec["gotInvoice"], rec["gotTax"] = j["got_invoice"], j["got_tax"]
    if j["inv_amt"] is not None:
        rec["invAmt"], rec["invZero"] = str(j["inv_amt"]), j["inv_zero"]
    if j["tax_amt"] is not None:
        rec["taxAmt"], rec["taxZero"] = str(j["tax_amt"]), j["tax_zero"]
        ysx, ynse, sjje = j["tax_parts"]
        rec["ysxssr"], rec["ynse"], rec["sjje"] = str(ysx), str(ynse), str(sjje)
    rec["total"], rec["isZero"] = str(j["total"]), j["is_zero"]
    rec["outcome"] = "查得"
    if rec["bodyCode"] == "002" and not obtained(rec):
        rec["outcome"] = "未取得"
        for k in ("isZero", "invAmt", "taxAmt", "invZero", "taxZero", "total", "ysxssr", "ynse", "sjje"):
            rec[k] = None
    return rec


def _run_one(task, cfg):
    credit, dt = task["credit"], task["dataType"]
    status, raw, elapsed, err = query(credit, dt)
    if err or status >= 500 or status == 0:
        time.sleep(1.5)
        status, raw, elapsed, err = query(credit, dt)
    if raw:
        (cfg.raw / f"{dt}_{credit}.json").write_text(raw, encoding="utf-8")
    rec = interpret(credit, dt, status, raw, elapsed, err, cfg.win_from, cfg.win_to)
    rec.update(request=task["request"], name=task["name"], block=task["block"])
    with _LOCK:
        with cfg.progress.open("a", encoding="utf-8") as f:
            f.write(json.dumps(rec, ensure_ascii=False) + "\n")
    return rec


def _load_done(cfg):
    done = {}
    if cfg.progress.exists():
        for line in cfg.progress.read_text(encoding="utf-8").splitlines():
            line = line.strip()
            if line:
                r = json.loads(line)
                done[r["credit"]] = r
    return done


# ---------- excel ----------
HEADERS = ["税号", "企业名称", "计费维度", "查询结果", "是否查得", "实得维度",
           "近12月开票金额", "近12月应税销售收入", "近12月应纳税额", "近12月实缴金额",
           "近12月税务金额", "近12月总金额", "近12月总金额是否为0", "说明",
           "bodyCode", "reqid", "耗时秒"]
WIDTHS = [22, 30, 10, 10, 10, 12, 16, 18, 16, 16, 16, 16, 18, 30, 10, 20, 8]
HEAD_FONT = Font(bold=True, color="FFFFFF")
HEAD_FILL = PatternFill("solid", fgColor="1F4E79")
ZERO_FILL = PatternFill("solid", fgColor="FCE4D6")
NF_FILL = PatternFill("solid", fgColor="EDEDED")
_thin = Side(style="thin", color="BFBFBF")
BORD = Border(left=_thin, right=_thin, top=_thin, bottom=_thin)


def _dec(v):
    if v in (None, ""):
        return None
    try:
        return Decimal(str(v))
    except InvalidOperation:
        return None


def _money(v):
    d = _dec(v)
    return float(d) if d is not None else ""


def _yn(v):
    return "" if v is None else ("是" if v else "否")


def _got(rec):
    if rec.get("outcome") != "查得":
        return ""
    gi, gt = rec.get("gotInvoice"), rec.get("gotTax")
    return "票税" if gi and gt else ("票" if gi else ("税" if gt else ""))


def _advice(rec):
    o = rec.get("outcome")
    if o != "查得":
        return {"查无": "查无数据", "未取得": "未取得有效数据",
                "失败": "调用失败：" + (rec.get("errorMsg") or "")}.get(o, o)
    dt = rec.get("dataType")
    if dt == "invoice":
        return "近12月发票金额≤0" if rec.get("isZero") else "近12月发票金额>0"
    if dt == "tax":
        return "近12月税务金额≤0" if rec.get("isZero") else "近12月税务金额>0"
    iz, tz = rec.get("invZero"), rec.get("taxZero")
    if iz and tz:
        return "发票、税务近12月金额均≤0"
    if iz:
        return "仅发票近12月金额≤0（税务>0）"
    if tz:
        return "仅税务近12月金额≤0（发票>0）"
    return "发票、税务近12月金额均>0"


def _row(rec):
    return [rec["credit"], rec.get("company") or rec.get("name") or "",
            TYPE_LABEL.get(rec["dataType"], rec["dataType"]),
            rec.get("outcome"), "查得" if rec.get("outcome") == "查得" else "未查得", _got(rec),
            _money(rec.get("invAmt")), _money(rec.get("ysxssr")), _money(rec.get("ynse")),
            _money(rec.get("sjje")), _money(rec.get("taxAmt")), _money(rec.get("total")),
            _yn(rec.get("isZero")), _advice(rec),
            rec.get("bodyCode") or "", rec.get("reqid") or "", rec.get("elapsed") or ""]


def _add_sheet(wb, title, rows, highlight=True):
    if title in wb.sheetnames:
        del wb[title]
    ws = wb.create_sheet(title)
    ws.append(HEADERS)
    for c in ws[1]:
        c.font, c.fill, c.border = HEAD_FONT, HEAD_FILL, BORD
        c.alignment = Alignment(horizontal="center", vertical="center")
    for rec in rows:
        ws.append(_row(rec))
        fill = None
        if highlight and rec.get("outcome") == "查得" and rec.get("isZero"):
            fill = ZERO_FILL
        elif highlight and rec.get("outcome") != "查得":
            fill = NF_FILL
        for c in ws[ws.max_row]:
            c.border = BORD
            if fill:
                c.fill = fill
    for i, w in enumerate(WIDTHS, 1):
        ws.column_dimensions[get_column_letter(i)].width = w
    ws.freeze_panes = "A2"
    ws.auto_filter.ref = f"A1:{get_column_letter(len(HEADERS))}{ws.max_row}"
    return ws


def write_excel(cfg, tasks, records):
    wb = load_workbook(cfg.src)
    by = {r["credit"]: r for r in records}
    ordered = [by[t["credit"]] for t in tasks if t["credit"] in by]
    found = [r for r in ordered if r.get("outcome") == "查得"]
    zero = [r for r in found if r.get("isZero")]
    notfound = [r for r in ordered if r.get("outcome") != "查得"]
    cnt = lambda o: sum(1 for r in ordered if r.get("outcome") == o)

    _add_sheet(wb, "近12月金额为0", zero)
    _add_sheet(wb, "近12月核查明细", ordered)

    sm = wb["对账单"]
    start = cfg.summary_start
    bold = Font(bold=True)
    title_font = Font(bold=True, color="FFFFFF")
    title_fill = PatternFill("solid", fgColor="1F4E79")
    head_fill = PatternFill("solid", fgColor="D6DCE4")
    lines = [
        (f"近12月金额核查（账户 {APP_KEY}，窗口 {cfg.window_label}）", ""),
        ("计费税号去重数", len(ordered)),
        ("查得", len(found)),
        ("未查得（查无/未取得/失败）", len(notfound)),
        ("　└ 查无", cnt("查无")),
        ("　└ 未取得", cnt("未取得")),
        ("　└ 调用失败", cnt("失败")),
        ("近12月总金额为0的税号（查得且总金额≤0）", len(zero)),
        ("　└ 其中仅票为0", sum(1 for r in zero if r["dataType"] == "invoice")),
        ("　└ 其中仅税为0", sum(1 for r in zero if r["dataType"] == "tax")),
        ("　└ 其中票税均为0", sum(1 for r in zero if r["dataType"] == "both")),
        ("说明", "维度跟随账单计费维度；总金额=票(开票金额)+税(应税收入/应纳税额/实缴金额正数部分)。详见「近12月核查明细」「近12月金额为0」。"),
    ]
    key = ("计费税号去重数", "查得", "未查得（查无/未取得/失败）", "近12月总金额为0的税号（查得且总金额≤0）")
    for i, (k, v) in enumerate(lines):
        r = start + i
        ck, cv = sm.cell(r, 2, k), sm.cell(r, 3, v)
        if i == 0:
            ck.font = cv.font = title_font
            ck.fill = cv.fill = title_fill
        elif k in key:
            ck.font = cv.font = bold
            ck.fill = head_fill
        if "总金额为0的税号" in str(k):
            cv.fill = ZERO_FILL
            cv.font = Font(bold=True, color="C00000")

    wb.save(cfg.out)
    print("saved", cfg.out)
    print(f"计费税号 {len(ordered)} | 查得 {len(found)} 未查得 {len(notfound)} "
          f"(查无{cnt('查无')}/未取得{cnt('未取得')}/失败{cnt('失败')}) | 近12月金额为0 {len(zero)}")
    print(f"  金额为0拆分: 仅票{sum(1 for r in zero if r['dataType']=='invoice')} "
          f"仅税{sum(1 for r in zero if r['dataType']=='tax')} "
          f"票税{sum(1 for r in zero if r['dataType']=='both')}")


def run(cfg):
    cfg.work.mkdir(parents=True, exist_ok=True)
    cfg.raw.mkdir(parents=True, exist_ok=True)
    tasks = cfg.load_tasks(cfg)
    print(f"tasks {len(tasks)} window {cfg.win_from}-{cfg.win_to} base {BASE}")
    done = _load_done(cfg)
    pending = [t for t in tasks
               if not (done.get(t["credit"]) and done[t["credit"]].get("outcome") in {"查得", "查无", "未取得"}
                       and done[t["credit"]].get("dataType") == t["dataType"])]
    print(f"done {len(tasks) - len(pending)} pending {len(pending)}")
    if pending:
        with ThreadPoolExecutor(max_workers=WORKERS) as pool:
            futs = {pool.submit(_run_one, t, cfg): t for t in pending}
            n = 0
            for fut in as_completed(futs):
                n += 1
                try:
                    rec = fut.result()
                except Exception as e:
                    rec = {"credit": futs[fut]["credit"], "outcome": "失败", "error": str(e)}
                if n % 25 == 0 or rec.get("outcome") == "失败":
                    print(f"[{n}/{len(pending)}] {rec.get('credit')} {rec.get('dataType')} "
                          f"{rec.get('outcome')} total={rec.get('total')} "
                          f"{rec.get('errorMsg') or ''} {rec.get('elapsed')}s", flush=True)
    done = _load_done(cfg)
    records = []
    for t in tasks:
        rec = done.get(t["credit"])
        if rec:
            rec.update(dataType=t["dataType"], request=t["request"],
                       name=rec.get("name") or t["name"], block=rec.get("block") or t["block"])
            records.append(rec)
    write_excel(cfg, tasks, records)
