# -*- coding: utf-8 -*-
"""冠辰科技(惠众)10月对账单：按计费维度重查每个税号，判定近12个月总金额是否为0。

- 账户：appKey=j92eme33uipj / appSecret（本次查询专用账户，已开通发票+税务）。
- 维度跟随账单：只计费票 -> invoice；只计费税 -> tax；票税 -> both。
- 窗口：[请求月-12, 请求月] = [2025-10, 2026-10]，与系统 zerorule v1 一致（锚定账单月10月）。
- 金额口径：票=窗口内 kphzxxList.kpje 合计；税=应税销售收入/应纳税额/实缴金额各自合计的正数部分之和；
  票税=两者相加。总金额<=0 记为「近12月金额为0」。查无、未取得(002无实得)不算金额为0。
- 查得/未查得：body.code=001 且本次实得对应维度 -> 查得；其余(999查无 / 002未取得 / 失败) -> 未查得。
"""
import hashlib
import json
import sys
import threading
import time
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor, as_completed
from collections import defaultdict
from decimal import Decimal, InvalidOperation
from pathlib import Path

from openpyxl import load_workbook
from openpyxl.styles import Alignment, Font, PatternFill, Border, Side
from openpyxl.utils import get_column_letter

sys.stdout.reconfigure(encoding="utf-8")

SRC = Path(r"c:\Users\XMMXYY\Documents\xwechat_files\wxid_t7vgbilml8m511_c2b2\msg\file\2026-11\冠辰科技-月度对账单(1).xlsx")
OUT = SRC.with_name("冠辰科技-月度对账单(1)-近12月金额核查.xlsx")
WORK = Path(r"c:\workspace\DataHub_SWFP\.scratch\ganchen_recall")
RAW = WORK / "raw"
PROGRESS = WORK / "progress.jsonl"
LOCK = threading.Lock()

APP_KEY = "j92eme33uipj"
SECRET = "4c08fd3cafb7615e99506ae3e5d694f4"
BASE = "http://www.aiszcloud.cn:8081"
PATH = "/v1/openapi/zlx/querySrmxSWFP"
WORKERS = 6
TIMEOUT = 40
WIN_FROM, WIN_TO = 202510, 202610  # 账单月10月，[请求月-12, 请求月]

TYPE_LABEL = {"invoice": "票", "tax": "税", "both": "票税"}


def norm(t):
    return str(t).strip().upper()


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


def sum_list(items, amount_keys, month_keys):
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
        if ym == 0 or ym < WIN_FROM or ym > WIN_TO:
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


def judge(rng, data_type):
    inv = (rng or {}).get("发票数据聚合") or {}
    tax = (rng or {}).get("税务数据聚合") or {}
    scope = (rng or {}).get("dataScope") or {}
    (inv_amt,) = sum_list(inv.get("kphzxxList"), ["kpje"], ["ssyf", "kpqj"])
    ysx, ynse = sum_list(tax.get("sbsjList"), ["ysxssr", "ynse"], ["sssjz", "sssjq", "sbrq"])
    (sjje,) = sum_list(tax.get("zsbxxList"), ["sjje"], ["sssjz", "sssjq", "rkrq"])
    tax_parts = [ysx, ynse, sjje]
    tax_amt = sum((p for p in tax_parts if p > 0), Decimal(0))
    want_inv = data_type in ("invoice", "both")
    want_tax = data_type in ("tax", "both")
    total = Decimal(0)
    if want_inv:
        total += inv_amt
    if want_tax:
        total += tax_amt
    return {
        "company": company_name(inv, tax),
        "got_invoice": bool(scope.get("发票")),
        "got_tax": bool(scope.get("税务")),
        "inv_amt": inv_amt if want_inv else None,
        "tax_amt": tax_amt if want_tax else None,
        "tax_parts": [ysx, ynse, sjje] if want_tax else None,
        "inv_zero": (inv_amt <= 0) if want_inv else None,
        "tax_zero": all(p <= 0 for p in tax_parts) if want_tax else None,
        "total": total,
        "is_zero": total <= 0,
    }


def query(credit, data_type):
    body = {"creditCode": credit, "dataType": data_type}
    payload = {"encryptionType": 1, "appKey": APP_KEY, "sign": sign(body), "body": body}
    data = json.dumps(payload, ensure_ascii=False).encode("utf-8")
    req = urllib.request.Request(
        BASE + PATH, data=data,
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


def interpret(credit, data_type, status, raw, elapsed, err):
    rec = {"credit": credit, "dataType": data_type, "httpStatus": status,
           "elapsed": round(elapsed, 2), "error": err, "errorCode": "", "errorMsg": "",
           "bodyCode": "", "bodyMsg": "", "reqid": "", "company": "",
           "gotInvoice": None, "gotTax": None, "invAmt": None, "taxAmt": None,
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
    head = obj.get("head") or {}
    body = obj.get("body") or {}
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
    j = judge(rng, data_type)
    rec["company"] = j["company"]
    rec["gotInvoice"] = j["got_invoice"]
    rec["gotTax"] = j["got_tax"]
    if j["inv_amt"] is not None:
        rec["invAmt"] = str(j["inv_amt"])
        rec["invZero"] = j["inv_zero"]
    if j["tax_amt"] is not None:
        rec["taxAmt"] = str(j["tax_amt"])
        rec["taxZero"] = j["tax_zero"]
        ysx, ynse, sjje = j["tax_parts"]
        rec["ysxssr"], rec["ynse"], rec["sjje"] = str(ysx), str(ynse), str(sjje)
    rec["total"] = str(j["total"])
    rec["isZero"] = j["is_zero"]
    rec["outcome"] = "查得"
    # 002 且本次没有实得维度：不能当作金额为0
    if rec["bodyCode"] == "002" and not obtained(rec):
        rec["outcome"] = "未取得"
        for k in ("isZero", "invAmt", "taxAmt", "invZero", "taxZero", "total", "ysxssr", "ynse", "sjje"):
            rec[k] = None
    return rec


def run_one(task):
    credit, dt = task["credit"], task["dataType"]
    status, raw, elapsed, err = query(credit, dt)
    if err or status >= 500 or status == 0:
        time.sleep(1.5)
        status, raw, elapsed, err = query(credit, dt)
    if raw:
        (RAW / f"{dt}_{credit}.json").write_text(raw, encoding="utf-8")
    rec = interpret(credit, dt, status, raw, elapsed, err)
    rec.update(request=task["request"], name=task["name"], block=task["block"])
    with LOCK:
        with PROGRESS.open("a", encoding="utf-8") as f:
            f.write(json.dumps(rec, ensure_ascii=False) + "\n")
    return rec


def load_tasks():
    wb = load_workbook(SRC, read_only=True, data_only=True)
    dims = defaultdict(lambda: {"inv": False, "tax": False})
    name, blocks = {}, defaultdict(set)
    for sh, blk in (("10月明细A", "A"), ("10月明细C", "C")):
        ws = wb[sh]
        it = ws.iter_rows(values_only=True)
        hdr = list(next(it))
        ix = {c: i for i, c in enumerate(hdr)}
        for r in it:
            if not r or not r[ix["税号"]]:
                continue
            t = norm(r[ix["税号"]])
            if r[ix["计费票"]] in (1, "1", 1.0):
                dims[t]["inv"] = True
            if r[ix["计费税"]] in (1, "1", 1.0):
                dims[t]["tax"] = True
            if "企业名称" in ix and r[ix["企业名称"]]:
                name[t] = r[ix["企业名称"]]
            blocks[t].add(blk)
    tasks = []
    for t in dims:
        d = dims[t]
        dt = "both" if d["inv"] and d["tax"] else ("invoice" if d["inv"] else "tax")
        req = "票税" if dt == "both" else ("票" if dt == "invoice" else "税")
        tasks.append({"credit": t, "dataType": dt, "request": req,
                      "name": name.get(t, ""), "block": "、".join(sorted(blocks[t]))})
    tasks.sort(key=lambda x: x["credit"])
    return tasks


def load_done():
    done = {}
    if PROGRESS.exists():
        for line in PROGRESS.read_text(encoding="utf-8").splitlines():
            line = line.strip()
            if line:
                r = json.loads(line)
                done[r["credit"]] = r
    return done


def main():
    WORK.mkdir(parents=True, exist_ok=True)
    RAW.mkdir(parents=True, exist_ok=True)
    tasks = load_tasks()
    print(f"tasks {len(tasks)} window {WIN_FROM}-{WIN_TO} base {BASE}")
    done = load_done()
    pending = [t for t in tasks
               if not (done.get(t["credit"]) and done[t["credit"]].get("outcome") in {"查得", "查无", "未取得"}
                       and done[t["credit"]].get("dataType") == t["dataType"])]
    print(f"done {len(tasks) - len(pending)} pending {len(pending)}")
    if pending:
        with ThreadPoolExecutor(max_workers=WORKERS) as pool:
            futs = {pool.submit(run_one, t): t for t in pending}
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
    done = load_done()
    records = []
    for t in tasks:
        rec = done.get(t["credit"])
        if rec:
            rec.update(dataType=t["dataType"], request=t["request"],
                       name=rec.get("name") or t["name"], block=rec.get("block") or t["block"])
            records.append(rec)
    from ganchen_report import write_excel
    write_excel(tasks, records)


if __name__ == "__main__":
    main()
