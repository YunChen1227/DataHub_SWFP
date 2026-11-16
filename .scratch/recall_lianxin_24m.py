# -*- coding: utf-8 -*-
"""Re-query 北京联信 9月/10月 账单里「查得」的税号，按 24 个月口径判定总金额是否 <= 0。

背景：这些「查得」记录当初没有存原始数据（账单里全部落在「数据库无记录（待人工）」
表），所以用操作手册的 swfp 聚合查询接口重新调一遍，拿到发票/税务原始聚合数据后，
按零数据规则 v1（但窗口放宽到 24 个月）重新计算总金额。总金额 <= 0 的，不能向下游收费。

口径（与 internal/domain/zerorule 一致，仅窗口 12→24 个月）：
  - 窗口：所属月份落在 [账单月-24, 账单月]（北京时间）闭区间内的条目；9 月账单锚定
    2026-09（窗口 2024-09..2026-09），10 月账单锚定 2026-10（窗口 2024-10..2026-10）。
  - 发票金额：窗口内 kphzxxList.kpje 合计。
  - 税务金额：窗口内 sbsjList.ysxssr、sbsjList.ynse、zsbxxList.sjje 各自合计后取正数部分之和。
  - 本次调用维度 = 实得维度（当初实际查得并计费的维度）：invoice/tax/invoice+tax -> invoice/tax/both。
  - 总金额 = 本次调用维度上各维度金额之和；总金额 <= 0 即「24个月总金额为0」。
  - 查无、以及 002 且该维度本次没有实得数据的，不算金额为 0（交人工）。
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
from decimal import Decimal, InvalidOperation
from pathlib import Path

from openpyxl import load_workbook
from openpyxl.styles import Alignment, Font, PatternFill
from openpyxl.utils import get_column_letter

sys.stdout.reconfigure(encoding="utf-8")

DL = Path(r"c:\Users\XMMXYY\Downloads")
FILES = {
    "sep": {
        "src": DL / "下游账单-北京联信征信咨询有限责任公司-2026-09-01至2026-09-30.xlsx",
        "out": DL / "下游账单-北京联信征信咨询有限责任公司-2026-09-01至2026-09-30-24月零金额.xlsx",
        "anchor": 202609,
    },
    "oct": {
        "src": DL / "下游账单-北京联信征信咨询有限责任公司-2026-10-01至2026-10-31.xlsx",
        "out": DL / "下游账单-北京联信征信咨询有限责任公司-2026-10-01至2026-10-31-24月零金额.xlsx",
        "anchor": 202610,
    },
}
SHEET = "数据库无记录（待人工）"

WORK = Path(r"c:\workspace\DataHub_SWFP\.scratch\lianxin_24m")
RAW = WORK / "raw"
PROGRESS = WORK / "progress.jsonl"
PROGRESS_LOCK = threading.Lock()

APP_KEY = os.environ["SWFP_APP_KEY"]
SECRET = os.environ["SWFP_SECRET"]
BASE = os.environ.get("RELAY_BASE_URL", "http://www.aiszcloud.cn:8081").rstrip("/")
PATH = "/v1/openapi/zlx/querySrmxSWFP"
WORKERS = int(os.environ.get("SWFP_WORKERS", "4"))
TIMEOUT = int(os.environ.get("SWFP_TIMEOUT", "40"))

DIM_MAP = {"invoice": "invoice", "tax": "tax", "invoice+tax": "both"}
TYPE_LABEL = {"invoice": "票", "tax": "税", "both": "票税"}
MONTHS_BACK = 24


def sign(body: dict, secret: str) -> str:
    parts = []
    for k in sorted(body.keys()):
        v = body[k]
        if v is None or v == "":
            continue
        parts.append(f"{k}{v}")
    return hashlib.md5(("".join(parts) + secret).encode("utf-8")).hexdigest()


def window_for(anchor: int):
    y, m = anchor // 100, anchor % 100
    sm = m - MONTHS_BACK
    sy = y
    while sm <= 0:
        sm += 12
        sy -= 1
    return sy * 100 + sm, anchor


def year_month(v) -> int:
    if v is None or isinstance(v, bool):
        return 0
    if isinstance(v, (int, Decimal)):
        s = str(v)
    elif isinstance(v, float):
        s = format(v, "f")
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
    (inv_amt,) = sum_list(inv.get("kphzxxList"), ["kpje"], ["ssyf", "kpqj"], frm, to)
    ysx, ynse = sum_list(tax.get("sbsjList"), ["ysxssr", "ynse"], ["sssjz", "sssjq", "sbrq"], frm, to)
    (sjje,) = sum_list(tax.get("zsbxxList"), ["sjje"], ["sssjz", "sssjq", "rkrq"], frm, to)
    tax_parts = [ysx, ynse, sjje]
    tax_amt = sum((p for p in tax_parts if p > 0), Decimal(0))
    want_inv = data_type in ("invoice", "both")
    want_tax = data_type in ("tax", "both")
    inv_zero = inv_amt <= 0 if want_inv else None
    tax_zero = all(p <= 0 for p in tax_parts) if want_tax else None
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
        "tax_parts": tax_parts if want_tax else None,
        "inv_zero": inv_zero,
        "tax_zero": tax_zero,
        "total": total,
        "is_zero": total <= 0,
    }


def query(credit: str, data_type: str):
    body = {"creditCode": credit, "dataType": data_type}
    payload = {"encryptionType": 1, "appKey": APP_KEY, "sign": sign(body, SECRET), "body": body}
    data = json.dumps(payload, ensure_ascii=False).encode("utf-8")
    req = urllib.request.Request(
        BASE + PATH, data=data,
        headers={"Content-Type": "application/json; charset=utf-8"}, method="POST",
    )
    t0 = time.perf_counter()
    try:
        with urllib.request.urlopen(req, timeout=TIMEOUT) as resp:
            return resp.status, resp.read().decode("utf-8", errors="replace"), time.perf_counter() - t0, ""
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", errors="replace"), time.perf_counter() - t0, ""
    except Exception as e:
        return 0, "", time.perf_counter() - t0, f"{type(e).__name__}: {e}"


def obtained(rec) -> bool:
    dt = rec.get("dataType")
    gi, gt = rec.get("gotInvoice"), rec.get("gotTax")
    if dt == "invoice":
        return bool(gi)
    if dt == "tax":
        return bool(gt)
    if dt == "both":
        return bool(gi) or bool(gt)
    return False


def interpret(key, credit, data_type, frm, to, status, raw, elapsed, err):
    rec = {
        "key": key, "credit": credit, "dataType": data_type, "frm": frm, "to": to,
        "httpStatus": status, "elapsed": round(elapsed, 2), "error": err,
        "errorCode": "", "errorMsg": "", "logId": "", "bodyCode": "", "bodyMsg": "",
        "reqid": "", "company": "", "gotInvoice": None, "gotTax": None,
        "invAmt": None, "taxAmt": None, "ysxssr": None, "ynse": None, "sjje": None,
        "invZero": None, "taxZero": None, "total": None, "isZero": None, "outcome": "失败",
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
    rec["logId"] = str(head.get("logId") or "")
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
            rec["errorMsg"] = "range 不是合法 JSON"
            return rec
    if not isinstance(rng, dict):
        rec["outcome"] = "查无"
        return rec
    j = judge_amounts(rng, data_type, frm, to)
    rec["company"] = j["company"]
    rec["gotInvoice"] = j["got_invoice"]
    rec["gotTax"] = j["got_tax"]
    if j["inv_amt"] is not None:
        rec["invAmt"] = str(j["inv_amt"])
        rec["invZero"] = j["inv_zero"]
    if j["tax_amt"] is not None:
        rec["taxAmt"] = str(j["tax_amt"])
        rec["taxZero"] = j["tax_zero"]
        rec["ysxssr"], rec["ynse"], rec["sjje"] = (str(x) for x in j["tax_parts"])
    rec["total"] = str(j["total"])
    rec["isZero"] = j["is_zero"]
    rec["outcome"] = "查得"
    # 002 且本次请求维度没有实得数据：空列表求和得 0，不能当成金额为 0。
    if rec["bodyCode"] == "002" and not obtained(rec):
        rec["outcome"] = "未取得"
        for k in ("isZero", "invAmt", "taxAmt", "invZero", "taxZero", "total", "ysxssr", "ynse", "sjje"):
            rec[k] = None
    return rec


def append_progress(rec):
    with PROGRESS_LOCK:
        with PROGRESS.open("a", encoding="utf-8") as f:
            f.write(json.dumps(rec, ensure_ascii=False) + "\n")


def load_done():
    done = {}
    if PROGRESS.exists():
        for line in PROGRESS.read_text(encoding="utf-8").splitlines():
            line = line.strip()
            if line:
                r = json.loads(line)
                done[r["key"]] = r
    return done


def run_one(task):
    credit, data_type = task["credit"], task["dataType"]
    frm, to = task["frm"], task["to"]
    status, raw, elapsed, err = query(credit, data_type)
    if err or status >= 500 or status == 0 or (raw and '"002"' in raw):
        time.sleep(1.5)
        status, raw, elapsed, err = query(credit, data_type)
    if raw:
        (RAW / f"{task['key']}.json").write_text(raw, encoding="utf-8")
    rec = interpret(task["key"], credit, data_type, frm, to, status, raw, elapsed, err)
    append_progress(rec)
    return rec


def read_queried(src: Path):
    """读「数据库无记录（待人工）」表里的查得税号 -> (请求维度, 实得维度)."""
    wb = load_workbook(src, read_only=True, data_only=True)
    ws = wb[SHEET]
    rows = ws.iter_rows(values_only=True)
    header = next(rows)
    idx = {h: i for i, h in enumerate(header)}
    out = []
    for row in rows:
        if not row or not row[idx["税号"]]:
            continue
        cc = str(row[idx["税号"]]).strip()
        if str(row[idx["查得情况"]]).strip() != "查得":
            continue
        req = str(row[idx["请求维度"]] or "").strip()
        got = str(row[idx["实得维度"]] or "").strip()
        out.append({"credit": cc, "req": req, "got": got})
    wb.close()
    return out


def dim_to_type(got, req):
    d = got or req
    if d not in DIM_MAP:
        raise SystemExit(f"未知维度 {got!r}/{req!r}")
    return DIM_MAP[d]


# ---------- Excel 输出 ----------
HEADERS = [
    "税号", "企业名称", "请求维度", "实得维度(账单)", "本次调用", "调用结果", "本次实得",
    "近24月开票金额", "发票为0", "近24月应税销售收入", "近24月应纳税额", "近24月实缴金额",
    "近24月税务金额", "税务为0", "近24月总金额", "总金额<=0", "说明",
    "窗口", "httpStatus", "errorCode", "errorMsg", "bodyCode", "bodyMsg", "reqid", "logId", "耗时秒",
]
WIDTHS = [22, 30, 12, 14, 10, 10, 10, 16, 9, 20, 18, 18, 16, 9, 16, 11, 26, 16, 11, 11, 24, 10, 16, 20, 34, 9]


def money(v):
    if v is None or v == "":
        return ""
    return float(Decimal(v))


def yn(v):
    return "" if v is None else ("是" if v else "否")


def got_label(rec):
    if rec["outcome"] != "查得":
        return ""
    gi, gt = rec["gotInvoice"], rec["gotTax"]
    return "invoice+tax" if gi and gt else "invoice" if gi else "tax" if gt else ""


def advice(rec):
    if rec["outcome"] != "查得":
        return rec["outcome"]
    dt = rec["dataType"]
    if dt == "invoice":
        return "近24月发票金额<=0，不能向下游收费" if rec["isZero"] else "近24月发票金额>0，可收费"
    if dt == "tax":
        return "近24月税务金额<=0，不能向下游收费" if rec["isZero"] else "近24月税务金额>0，可收费"
    iz, tz = rec["invZero"], rec["taxZero"]
    if iz and tz:
        return "发票、税务近24月均<=0，不能向下游收费"
    return "总金额<=0，不能向下游收费" if rec["isZero"] else "总金额>0，可收费"


def row_of(rec, entry):
    return [
        rec["credit"], rec.get("company") or "", entry["req"], entry["got"],
        TYPE_LABEL.get(rec["dataType"], rec["dataType"]), rec["outcome"], got_label(rec),
        money(rec.get("invAmt")), yn(rec.get("invZero")),
        money(rec.get("ysxssr")), money(rec.get("ynse")), money(rec.get("sjje")),
        money(rec.get("taxAmt")), yn(rec.get("taxZero")),
        money(rec.get("total")), yn(rec.get("isZero")), advice(rec),
        f'{rec.get("frm")}-{rec.get("to")}',
        rec.get("httpStatus") or "", rec.get("errorCode") or "", rec.get("errorMsg") or "",
        rec.get("bodyCode") or "", rec.get("bodyMsg") or "", rec.get("reqid") or "",
        rec.get("logId") or "", rec.get("elapsed") or "",
    ]


def write_file(name, cfg, entries, cache):
    wb = load_workbook(cfg["src"])
    for s in ("24月重调明细", "24月金额为0"):
        if s in wb.sheetnames:
            del wb[s]
    header_font = Font(bold=True, color="FFFFFF")
    header_fill = PatternFill("solid", fgColor="1F4E79")
    zero_fill = PatternFill("solid", fgColor="FCE4D6")

    detail, zero = [], []
    for e in entries:
        rec = cache[e["key"]]
        detail.append((rec, e))
        if rec["outcome"] == "查得" and rec.get("isZero"):
            zero.append((rec, e))

    def add_sheet(title, rows, note):
        ws = wb.create_sheet(title)
        ws.append([note] + [""] * (len(HEADERS) - 1))
        ws.append(HEADERS)
        for cell in ws[2]:
            cell.font = header_font
            cell.fill = header_fill
            cell.alignment = Alignment(horizontal="center")
        for rec, e in rows:
            ws.append(row_of(rec, e))
            if rec["outcome"] == "查得" and rec.get("isZero"):
                for cell in ws[ws.max_row]:
                    cell.fill = zero_fill
        for i, w in enumerate(WIDTHS, 1):
            ws.column_dimensions[get_column_letter(i)].width = w
        ws.freeze_panes = "A3"
        ws.auto_filter.ref = f"A2:{get_column_letter(len(HEADERS))}{ws.max_row}"
        return ws

    frm, to = window_for(cfg["anchor"])
    note_all = (f"对账单里「查得」的税号重新调 swfp 接口，按近24个月口径（窗口 {frm}-{to}，"
                f"即账单月及之前24个月，含账单月，北京时间）判定。本次调用维度=实得维度。"
                f"总金额<=0 的行见「24月金额为0」表，并已高亮。")
    note_zero = (f"以下税号近24月总金额<=0（窗口 {frm}-{to}），按零数据规则不能向下游收费。"
                 f"查无/未取得的不在此表。")
    add_sheet("24月重调明细", detail, note_all)
    add_sheet("24月金额为0", zero, note_zero)
    wb.save(cfg["out"])
    print(f"[{name}] 写出 {cfg['out'].name}: 查得明细 {len(detail)} 行，其中总金额<=0 {len(zero)} 行")
    return len(detail), len(zero)


def main():
    WORK.mkdir(parents=True, exist_ok=True)
    RAW.mkdir(parents=True, exist_ok=True)

    per_file = {}
    tasks = {}
    for name, cfg in FILES.items():
        frm, to = window_for(cfg["anchor"])
        entries = read_queried(cfg["src"])
        for e in entries:
            dt = dim_to_type(e["got"], e["req"])
            key = f"{dt}_{e['credit']}_{frm}-{to}"
            e["key"] = key
            e["dataType"] = dt
            tasks[key] = {"key": key, "credit": e["credit"], "dataType": dt, "frm": frm, "to": to}
        per_file[name] = entries
        print(f"[{name}] 查得税号 {len(entries)} 个 窗口 {frm}-{to}")

    print(f"去重后待调 {len(tasks)} 个 (credit,dataType,window) 组合 base={BASE} workers={WORKERS}")
    done = load_done()
    pending = [t for k, t in tasks.items()
               if not (done.get(k) and done[k].get("outcome") in {"查得", "查无"})]
    print(f"已完成 {len(tasks) - len(pending)} 待调 {len(pending)}")

    if pending:
        with ThreadPoolExecutor(max_workers=WORKERS) as pool:
            futs = {pool.submit(run_one, t): t for t in pending}
            n = 0
            for fut in as_completed(futs):
                n += 1
                t = futs[fut]
                try:
                    rec = fut.result()
                except Exception as ex:
                    rec = {"credit": t["credit"], "outcome": "失败", "error": str(ex)}
                print(f"[{n}/{len(pending)}] {rec.get('credit')} {rec.get('dataType')} "
                      f"{rec.get('outcome')} total={rec.get('total')} code={rec.get('bodyCode')} "
                      f"{rec.get('errorMsg') or ''} {rec.get('elapsed')}s", flush=True)

    cache = load_done()
    missing = [k for k in tasks if k not in cache]
    if missing:
        print("MISSING", len(missing), missing[:5])

    total_zero = 0
    for name, cfg in FILES.items():
        _, z = write_file(name, cfg, per_file[name], cache)
        total_zero += z
    print(f"完成。两文件合计 24月总金额<=0 行数（可能含跨月重复税号）= {total_zero}")


if __name__ == "__main__":
    main()
