# -*- coding: utf-8 -*-
"""Re-query 北京联信 downstream-bill tax numbers and flag 12-month amounts <= 0.

Call type follows the bill:
  票明细        -> dataType=invoice
  税明细        -> dataType=tax
  票税明细      -> dataType=both

12-month window matches zerorule v1: Beijing [request month-12, request month].
Amounts are read from the downstream contract (发票数据聚合 / 税务数据聚合),
which is the flattened form of the same fields zerorule reads upstream.
"""
import hashlib
import json
import os
import sys
import threading
import time
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor, as_completed
from datetime import datetime
from decimal import Decimal, InvalidOperation
from pathlib import Path

from openpyxl import load_workbook
from openpyxl.styles import Alignment, Font, PatternFill
from openpyxl.utils import get_column_letter

sys.stdout.reconfigure(encoding="utf-8")

SRC = Path(r"c:\Users\XMMXYY\Downloads\下游账单-北京联信征信咨询有限责任公司-2026-09-01至2026-10-31.xlsx")
OUT = Path(r"c:\Users\XMMXYY\Downloads\下游账单-北京联信征信咨询有限责任公司-2026-09-01至2026-10-31-重调统计.xlsx")
WORK = Path(r"c:\workspace\DataHub_SWFP\.scratch\lianxin_recall")
RAW = WORK / "raw"
PROGRESS = WORK / "progress.jsonl"
PROGRESS_LOCK = threading.Lock()

APP_KEY = os.environ["SWFP_APP_KEY"]
SECRET = os.environ["SWFP_SECRET"]
BASE = os.environ.get("RELAY_BASE_URL", "http://www.aiszcloud.cn:8081").rstrip("/")
PATH = "/v1/openapi/zlx/querySrmxSWFP"
WORKERS = int(os.environ.get("SWFP_WORKERS", "4"))
TIMEOUT = int(os.environ.get("SWFP_TIMEOUT", "40"))

TYPE_MAP = {"invoice": "invoice", "tax": "tax", "invoice+tax": "both"}
TYPE_LABEL = {"invoice": "票", "tax": "税", "both": "票税"}


def sign(body: dict, secret: str) -> str:
    parts = []
    for k in sorted(body.keys()):
        v = body[k]
        if v is None or v == "":
            continue
        parts.append(f"{k}{v}")
    return hashlib.md5(("".join(parts) + secret).encode("utf-8")).hexdigest()


def year_month(v) -> int:
    if v is None:
        return 0
    if isinstance(v, bool):
        return 0
    if isinstance(v, (int, float, Decimal)):
        s = format(v, "f") if isinstance(v, float) else str(v)
    else:
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


def window_now():
    # Beijing today. [month-12, month] inclusive, same as zerorule.window.
    now = datetime.utcnow()
    # UTC+8
    t = datetime.utcfromtimestamp(datetime.utcnow().timestamp() + 8 * 3600)
    y, m = t.year, t.month
    start_m = m - 12
    start_y = y
    while start_m <= 0:
        start_m += 12
        start_y -= 1
    return start_y * 100 + start_m, y * 100 + m


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
                name = str(info.get("nsrmc") or "").strip()
                if name:
                    return name
    return ""


def judge_amounts(rng: dict, data_type: str, frm: int, to: int):
    inv = (rng or {}).get("发票数据聚合") or {}
    tax = (rng or {}).get("税务数据聚合") or {}
    scope = (rng or {}).get("dataScope") or {}
    inv_sums = sum_list(inv.get("kphzxxList"), ["kpje"], ["ssyf", "kpqj"], frm, to)
    ysx, ynse = sum_list(
        tax.get("sbsjList"), ["ysxssr", "ynse"], ["sssjz", "sssjq", "sbrq"], frm, to
    )
    (sjje,) = sum_list(tax.get("zsbxxList"), ["sjje"], ["sssjz", "sssjq", "rkrq"], frm, to)
    inv_amt = inv_sums[0]
    tax_parts = [ysx, ynse, sjje]
    tax_amt = sum((p for p in tax_parts if p > 0), Decimal(0))
    want_inv = data_type in ("invoice", "both")
    want_tax = data_type in ("tax", "both")
    inv_zero = inv_amt <= 0 if want_inv else None
    tax_zero = all(p <= 0 for p in tax_parts) if want_tax else None
    # 总金额：只加本次调用维度。税务侧用展示口径（各项正数部分之和）。
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
        "inv_zero": inv_zero,
        "tax_zero": tax_zero,
        "total": total,
        "is_zero": total <= 0,
    }


def query(credit: str, data_type: str):
    body = {"creditCode": credit, "dataType": data_type}
    payload = {
        "encryptionType": 1,
        "appKey": APP_KEY,
        "sign": sign(body, SECRET),
        "body": body,
    }
    data = json.dumps(payload, ensure_ascii=False).encode("utf-8")
    req = urllib.request.Request(
        BASE + PATH,
        data=data,
        headers={"Content-Type": "application/json; charset=utf-8"},
        method="POST",
    )
    t0 = time.perf_counter()
    try:
        with urllib.request.urlopen(req, timeout=TIMEOUT) as resp:
            raw = resp.read().decode("utf-8", errors="replace")
            return resp.status, raw, time.perf_counter() - t0, ""
    except urllib.error.HTTPError as e:
        raw = e.read().decode("utf-8", errors="replace")
        return e.code, raw, time.perf_counter() - t0, ""
    except Exception as e:
        return 0, "", time.perf_counter() - t0, f"{type(e).__name__}: {e}"


def load_tasks():
    wb = load_workbook(SRC, read_only=True, data_only=True)
    seen = {}
    order = []
    for sheet, kind in (("票明细", "invoice"), ("税明细", "tax"), ("票税明细", "invoice+tax")):
        ws = wb[sheet]
        rows = ws.iter_rows(values_only=True)
        header = next(rows)
        idx = {h: i for i, h in enumerate(header)}
        for row in rows:
            if not row or not row[idx["税号"]]:
                continue
            cc = str(row[idx["税号"]]).strip()
            req = str(row[idx["请求维度"]] or "").strip() or kind
            if cc not in seen:
                seen[cc] = req
                order.append(cc)
            elif seen[cc] != req:
                raise SystemExit(f"税号 {cc} 出现了多种请求维度: {seen[cc]} / {req}")
    tasks = []
    for cc in order:
        req = seen[cc]
        if req not in TYPE_MAP:
            raise SystemExit(f"未知请求维度 {req} @ {cc}")
        tasks.append({"credit": cc, "request": req, "dataType": TYPE_MAP[req]})
    return tasks


def load_done():
    done = {}
    if not PROGRESS.exists():
        return done
    for line in PROGRESS.read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if not line:
            continue
        rec = json.loads(line)
        done[rec["credit"]] = rec
    return done


def append_progress(rec):
    line = json.dumps(rec, ensure_ascii=False) + "\n"
    with PROGRESS_LOCK:
        with PROGRESS.open("a", encoding="utf-8") as f:
            f.write(line)


def interpret(credit, data_type, status, raw, elapsed, err, frm, to):
    rec = {
        "credit": credit,
        "dataType": data_type,
        "httpStatus": status,
        "elapsed": round(elapsed, 2),
        "error": err,
        "errorCode": "",
        "errorMsg": "",
        "bodyCode": "",
        "bodyMsg": "",
        "reqid": "",
        "company": "",
        "gotInvoice": None,
        "gotTax": None,
        "invAmt": None,
        "taxAmt": None,
        "ysxssr": None,
        "ynse": None,
        "sjje": None,
        "invZero": None,
        "taxZero": None,
        "total": None,
        "isZero": None,
        "outcome": "失败",
    }
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
        rec["outcome"] = "失败"
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
            rec["outcome"] = "失败"
            rec["errorMsg"] = "range 不是合法 JSON"
            return rec
    if not isinstance(rng, dict):
        rec["outcome"] = "查无"
        return rec
    judged = judge_amounts(rng, data_type, frm, to)
    rec["company"] = judged["company"]
    rec["gotInvoice"] = judged["got_invoice"]
    rec["gotTax"] = judged["got_tax"]
    if judged["inv_amt"] is not None:
        rec["invAmt"] = str(judged["inv_amt"])
        rec["invZero"] = judged["inv_zero"]
    if judged["tax_amt"] is not None:
        rec["taxAmt"] = str(judged["tax_amt"])
        rec["taxZero"] = judged["tax_zero"]
        ysx, ynse, sjje = judged["tax_parts"]
        rec["ysxssr"] = str(ysx)
        rec["ynse"] = str(ynse)
        rec["sjje"] = str(sjje)
    rec["total"] = str(judged["total"])
    rec["isZero"] = judged["is_zero"]
    rec["outcome"] = "查得"
    return normalize(rec)


def run_one(task, frm, to):
    credit = task["credit"]
    data_type = task["dataType"]
    status, raw, elapsed, err = query(credit, data_type)
    # one retry on transport failure or gateway 5xx
    if err or status >= 500 or status == 0:
        time.sleep(1.5)
        status, raw, elapsed, err = query(credit, data_type)
    safe = f"{data_type}_{credit}.json"
    if raw:
        (RAW / safe).write_text(raw, encoding="utf-8")
    rec = interpret(credit, data_type, status, raw, elapsed, err, frm, to)
    rec["request"] = task["request"]
    append_progress(rec)
    return rec


def obtained(rec) -> bool:
    dt = rec.get("dataType")
    got_inv, got_tax = rec.get("gotInvoice"), rec.get("gotTax")
    if dt == "invoice":
        return bool(got_inv)
    if dt == "tax":
        return bool(got_tax)
    if dt == "both":
        return bool(got_inv) or bool(got_tax)
    return False


def normalize(rec):
    """002 且请求维度没有实得数据时，空列表求和会变成 0，不能当成金额确认为 0。"""
    if rec.get("outcome") == "查得" and rec.get("bodyCode") == "002" and not obtained(rec):
        rec = dict(rec)
        rec["outcome"] = "未取得"
        rec["isZero"] = None
        rec["invAmt"] = None
        rec["taxAmt"] = None
        rec["invZero"] = None
        rec["taxZero"] = None
        rec["total"] = None
        rec["ysxssr"] = None
        rec["ynse"] = None
        rec["sjje"] = None
    return rec


def dec(v):
    if v is None or v == "":
        return None
    return Decimal(v)


def yn(v):
    if v is None:
        return ""
    return "是" if v else "否"


def money(v):
    d = dec(v)
    if d is None:
        return ""
    return float(d)


def got_label(rec):
    if rec["outcome"] != "查得":
        return ""
    inv, tax = rec["gotInvoice"], rec["gotTax"]
    if inv and tax:
        return "invoice+tax"
    if inv:
        return "invoice"
    if tax:
        return "tax"
    return ""


def advice(rec):
    if rec["outcome"] != "查得":
        return rec["outcome"]
    dt = rec["dataType"]
    if dt == "invoice":
        return "近12月发票金额<=0" if rec["isZero"] else "近12月发票金额>0"
    if dt == "tax":
        return "近12月税务金额<=0" if rec["isZero"] else "近12月税务金额>0"
    inv_z, tax_z = rec["invZero"], rec["taxZero"]
    if inv_z and tax_z:
        return "发票、税务近12月金额均为<=0"
    if inv_z:
        return "仅发票近12月金额<=0"
    if tax_z:
        return "仅税务近12月金额<=0"
    return "发票、税务近12月金额均>0"


def write_excel(tasks, records):
    wb = load_workbook(SRC)
    # drop sheets we may have written on a previous run of this file
    for name in ("近12月金额为0", "重调明细"):
        if name in wb.sheetnames:
            del wb[name]

    headers = [
        "税号", "企业名称", "原请求维度", "本次调用", "调用结果", "实得维度",
        "近12月开票金额", "发票为0", "近12月应税销售收入", "近12月应纳税额", "近12月实缴金额",
        "近12月税务金额", "税务为0", "前12月总金额", "总金额<=0", "说明",
        "httpStatus", "errorCode", "errorMsg", "bodyCode", "bodyMsg", "reqid", "耗时秒",
    ]
    widths = [22, 28, 14, 10, 10, 14, 16, 10, 18, 16, 16, 16, 10, 16, 12, 28, 12, 12, 24, 12, 16, 28, 10]

    def row_of(rec):
        return [
            rec["credit"], rec.get("company") or "",
            rec.get("request") or "", TYPE_LABEL.get(rec["dataType"], rec["dataType"]),
            rec["outcome"], got_label(rec),
            money(rec.get("invAmt")), yn(rec.get("invZero")),
            money(rec.get("ysxssr")), money(rec.get("ynse")), money(rec.get("sjje")),
            money(rec.get("taxAmt")), yn(rec.get("taxZero")),
            money(rec.get("total")), yn(rec.get("isZero")), advice(rec),
            rec.get("httpStatus") or "", rec.get("errorCode") or "", rec.get("errorMsg") or "",
            rec.get("bodyCode") or "", rec.get("bodyMsg") or "", rec.get("reqid") or "",
            rec.get("elapsed") or "",
        ]

    ordered = []
    by_credit = {r["credit"]: r for r in records}
    for t in tasks:
        rec = by_credit.get(t["credit"])
        if rec:
            ordered.append(rec)

    zero_rows = [r for r in ordered if r.get("outcome") == "查得" and r.get("isZero")]

    def add_sheet(title, rows):
        ws = wb.create_sheet(title)
        header_font = Font(bold=True, color="FFFFFF")
        header_fill = PatternFill("solid", fgColor="1F4E79")
        zero_fill = PatternFill("solid", fgColor="FCE4D6")
        ws.append(headers)
        for cell in ws[1]:
            cell.font = header_font
            cell.fill = header_fill
            cell.alignment = Alignment(horizontal="center")
        for rec in rows:
            ws.append(row_of(rec))
            if rec.get("isZero"):
                for cell in ws[ws.max_row]:
                    cell.fill = zero_fill
        for i, w in enumerate(widths, 1):
            ws.column_dimensions[get_column_letter(i)].width = w
        ws.auto_filter.ref = f"A1:{get_column_letter(len(headers))}{ws.max_row}"
        ws.freeze_panes = "A2"
        ws.auto_filter.ref = ws.dimensions
        return ws

    add_sheet("近12月金额为0", zero_rows)
    add_sheet("重调明细", ordered)

    # 汇总 sheet: append an overall block for this re-query.
    summary = wb["汇总"]
    frm, to = window_now()

    def count(pred):
        return sum(1 for r in ordered if pred(r))

    by_type = {}
    for dt in ("invoice", "tax", "both"):
        subset = [r for r in ordered if r["dataType"] == dt]
        by_type[dt] = {
            "n": len(subset),
            "found": sum(1 for r in subset if r["outcome"] == "查得"),
            "empty": sum(1 for r in subset if r["outcome"] == "查无"),
            "nodata": sum(1 for r in subset if r["outcome"] == "未取得"),
            "fail": sum(1 for r in subset if r["outcome"] == "失败"),
            "zero": sum(1 for r in subset if r["outcome"] == "查得" and r.get("isZero")),
            "positive": sum(1 for r in subset if r["outcome"] == "查得" and r.get("isZero") is False),
        }

    start = summary.max_row + 2
    bold = Font(bold=True)
    title_fill = PatternFill("solid", fgColor="1F4E79")
    head_fill = PatternFill("solid", fgColor="D6DCE4")

    lines = [
        ["重调统计"],
        ["说明", "按账单里每个税号原请求维度重调一次：只调过票的调 invoice，只调过税的调 tax，调过票税的调 both。同一税号多条历史记录只调一次。"],
        ["重调时间", datetime.now().strftime("%Y-%m-%d %H:%M:%S")],
        ["金额窗口（北京时间）", f"{frm} 至 {to}（请求月及之前 12 个月，含请求月，与零数据规则 v1 相同）"],
        ["总金额口径", "票：窗口内开票金额 kpje 合计。税：应税销售收入、应纳税额、实缴金额各自合计的正数部分之和。票税：两者相加。总金额<=0 的税号见「近12月金额为0」。查无、以及返回 002 且该维度没有实得数据的，不算金额为 0。"],
        [],
        ["类型", "重调税号数", "查得", "查无", "未取得", "失败", "前12月总金额<=0", "前12月总金额>0"],
    ]
    for dt in ("invoice", "tax", "both"):
        s = by_type[dt]
        lines.append([TYPE_LABEL[dt], s["n"], s["found"], s["empty"], s["nodata"], s["fail"], s["zero"], s["positive"]])
    lines.append([
        "合计",
        count(lambda r: True),
        count(lambda r: r["outcome"] == "查得"),
        count(lambda r: r["outcome"] == "查无"),
        count(lambda r: r["outcome"] == "未取得"),
        count(lambda r: r["outcome"] == "失败"),
        count(lambda r: r["outcome"] == "查得" and r.get("isZero")),
        count(lambda r: r["outcome"] == "查得" and r.get("isZero") is False),
    ])
    # both 的拆分，方便看「只是某一侧为 0」
    both = [r for r in ordered if r["dataType"] == "both" and r["outcome"] == "查得"]
    if both:
        lines.append([])
        lines.append(["票税拆分", "户数"])
        lines.append(["发票与税务均为<=0", sum(1 for r in both if r.get("invZero") and r.get("taxZero"))])
        lines.append(["仅发票<=0", sum(1 for r in both if r.get("invZero") and not r.get("taxZero"))])
        lines.append(["仅税务<=0", sum(1 for r in both if r.get("taxZero") and not r.get("invZero"))])
        lines.append(["两侧均>0", sum(1 for r in both if not r.get("invZero") and not r.get("taxZero"))])

    for i, line in enumerate(lines):
        for j, val in enumerate(line, 1):
            cell = summary.cell(start + i, j, val)
            if i == 0:
                cell.font = Font(bold=True, color="FFFFFF")
                cell.fill = title_fill
            elif line and line[0] in {"类型", "票税拆分", "合计"}:
                cell.font = bold
                cell.fill = head_fill

    wb.save(OUT)
    print("xlsx", OUT)
    print("zero", len(zero_rows), "total", len(ordered))
    for dt in ("invoice", "tax", "both"):
        print(dt, by_type[dt])


def main():
    WORK.mkdir(parents=True, exist_ok=True)
    RAW.mkdir(parents=True, exist_ok=True)
    tasks = load_tasks()
    frm, to = window_now()
    print(f"tasks {len(tasks)} window {frm}-{to} base {BASE} workers {WORKERS}")
    done = load_done()
    # drop failed rows so they are retried; keep 查得/查无
    pending = []
    for t in tasks:
        prev = done.get(t["credit"])
        if prev and prev.get("outcome") in {"查得", "查无"} and prev.get("dataType") == t["dataType"]:
            continue
        pending.append(t)
    print(f"already done {len(tasks) - len(pending)} pending {len(pending)}")

    if pending:
        with ThreadPoolExecutor(max_workers=WORKERS) as pool:
            futs = {pool.submit(run_one, t, frm, to): t for t in pending}
            n = 0
            for fut in as_completed(futs):
                n += 1
                t = futs[fut]
                try:
                    rec = fut.result()
                except Exception as e:
                    rec = {"credit": t["credit"], "outcome": "失败", "error": str(e)}
                print(
                    f"[{n}/{len(pending)}] {rec.get('credit')} {rec.get('dataType')} "
                    f"{rec.get('outcome')} total={rec.get('total')} "
                    f"code={rec.get('bodyCode')} {rec.get('errorMsg') or ''} {rec.get('elapsed')}s",
                    flush=True,
                )

    done = load_done()
    records = []
    missing = []
    for t in tasks:
        rec = done.get(t["credit"])
        if not rec:
            missing.append(t["credit"])
            continue
        rec["request"] = t["request"]
        rec["dataType"] = t["dataType"]
        records.append(normalize(rec))
    if missing:
        print("MISSING", len(missing))
    write_excel(tasks, records)


if __name__ == "__main__":
    main()
