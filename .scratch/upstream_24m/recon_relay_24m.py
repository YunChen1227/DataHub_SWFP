# -*- coding: utf-8 -*-
"""上游对账：对 4 个上游（证通/惠众/税友；凯盈云已直连单独处理）的「数据库无记录
（待人工）」查得税号，按 24 个月口径重调一遍，判定总金额是否 <= 0。

关键约束：**保证返回是对应上游的**。
做法：直连客户阿里云聚合接口 (www.aiszcloud.cn:8081) 按类目维度重调一遍——聚合器寻源
是确定性的，正常会命中同一个上游；但为了真正「保证」，每次重调都用 head.logId=requestId
下钻后台 upstream_call 明细 (GET /admin/api/swfp/audits/{requestId}/calls)，取 status=ok
那一源的 provider 作为**实际查得上游**。只有实际查得上游 == 本文件上游时，才把金额写进该
文件；若被更高优先级上游拦截（provider 不符）或查无，则标注交人工，绝不把别家数据写成本家。

金额口径与 internal/domain/zerorule 一致，仅窗口 12->24 个月（与下游重调脚本同口径）：
  - 窗口：所属月份落在 [账单月-24, 账单月]（北京时间）闭区间内的条目。
  - 发票金额：窗口内 kphzxxList.kpje 合计。
  - 税务金额：窗口内 sbsjList.ysxssr/ynse、zsbxxList.sjje 各自合计后取正数部分之和。
  - 总金额 = 本次类目维度金额；<= 0 即「24月总金额为0」，不需向该上游缴费。
"""
import base64
import hashlib
import hmac
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

OUT = Path(r"c:\workspace\DataHub_SWFP\.scratch\upstream_24m")
RAW = OUT / "raw"
PROGRESS = OUT / "relay_progress.jsonl"
PROGRESS_LOCK = threading.Lock()

APP_KEY = os.environ.get("SWFP_APP_KEY", "j92eme33uipj")
SECRET = os.environ.get("SWFP_SECRET", "edd726edad2da85f39285e5e81f21af5")
JWT_SECRET = os.environ.get("ADMIN_JWT_SECRET", "aishuzhiadminjwtsecret")
BASE = os.environ.get("RELAY_BASE_URL", "http://www.aiszcloud.cn:8081").rstrip("/")
VER = os.environ.get("RELAY_VERSION", "swfp")
QPATH = "/v1/openapi/zlx/querySrmxSWFP"
WORKERS = int(os.environ.get("SWFP_WORKERS", "4"))
TIMEOUT = int(os.environ.get("SWFP_TIMEOUT", "40"))

# 只处理这三个上游（凯盈云已直连单独处理，且聚合里优先级最低，易被拦截，不走聚合）。
UPSTREAMS = {"entcredit", "ctax", "sy"}
UP_CN = {"entcredit": "证通", "ctax": "惠众", "sy": "税友", "salesdata": "凯盈云"}
TYPE_LABEL = {"invoice": "票", "tax": "税", "both": "票税"}
MONTHS_BACK = 24

_admin_token = None
_admin_exp = 0
_token_lock = threading.Lock()


# ---------------- 签名 / JWT ----------------
def b64u(b):
    return base64.urlsafe_b64encode(b).decode().rstrip("=")


def admin_token():
    global _admin_token, _admin_exp
    with _token_lock:
        now = int(time.time())
        if _admin_token and now < _admin_exp - 120:
            return _admin_token
        h = b64u(json.dumps({"alg": "HS256", "typ": "JWT"}, separators=(",", ":")).encode())
        c = b64u(json.dumps({"sub": "admin", "exp": now + 3600, "iat": now},
                            separators=(",", ":")).encode())
        inp = f"{h}.{c}"
        sig = b64u(hmac.new(JWT_SECRET.encode(), inp.encode(), hashlib.sha256).digest())
        _admin_token, _admin_exp = f"{inp}.{sig}", now + 3600
        return _admin_token


def sign(body, secret):
    parts = [f"{k}{body[k]}" for k in sorted(body) if body[k] not in (None, "")]
    return hashlib.md5(("".join(parts) + secret).encode("utf-8")).hexdigest()


# ---------------- 金额口径（与下游重调脚本一致）----------------
def window_for(anchor):
    y, m = anchor // 100, anchor % 100
    sm, sy = m - MONTHS_BACK, y
    while sm <= 0:
        sm += 12
        sy -= 1
    return sy * 100 + sm, anchor


def year_month(v):
    if v is None or isinstance(v, bool):
        return 0
    if isinstance(v, (int, Decimal)):
        s = str(v)
    elif isinstance(v, float):
        s = format(v, "f")
    else:
        s = str(v).strip()
    digits = []
    for ch in s:
        if len(digits) >= 6:
            break
        if ch.isdigit():
            digits.append(ch)
        elif ch in "-/.":
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


def judge_amounts(rng, dim, frm, to):
    inv = (rng or {}).get("发票数据聚合") or {}
    tax = (rng or {}).get("税务数据聚合") or {}
    scope = (rng or {}).get("dataScope") or {}
    (inv_amt,) = sum_list(inv.get("kphzxxList"), ["kpje"], ["ssyf", "kpqj"], frm, to)
    ysx, ynse = sum_list(tax.get("sbsjList"), ["ysxssr", "ynse"], ["sssjz", "sssjq", "sbrq"], frm, to)
    (sjje,) = sum_list(tax.get("zsbxxList"), ["sjje"], ["sssjz", "sssjq", "rkrq"], frm, to)
    tax_parts = [ysx, ynse, sjje]
    tax_amt = sum((p for p in tax_parts if p > 0), Decimal(0))
    want_inv = dim in ("invoice", "both")
    want_tax = dim in ("tax", "both")
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
        "inv_zero": (inv_amt <= 0) if want_inv else None,
        "tax_zero": all(p <= 0 for p in tax_parts) if want_tax else None,
        "total": total,
        "is_zero": total <= 0,
    }


# ---------------- 网络 ----------------
def http_post(url, payload):
    data = json.dumps(payload, ensure_ascii=False).encode("utf-8")
    req = urllib.request.Request(url, data=data,
        headers={"Content-Type": "application/json; charset=utf-8"}, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=TIMEOUT) as r:
            return r.status, r.read().decode("utf-8", "replace"), ""
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace"), ""
    except Exception as e:
        return 0, "", f"{type(e).__name__}: {e}"


def admin_get(path):
    req = urllib.request.Request(BASE + path,
        headers={"Authorization": "Bearer " + admin_token()})
    try:
        with urllib.request.urlopen(req, timeout=TIMEOUT) as r:
            return r.status, json.loads(r.read().decode("utf-8", "replace")), ""
    except urllib.error.HTTPError as e:
        return e.code, None, e.read().decode("utf-8", "replace")[:200]
    except Exception as e:
        return 0, None, f"{type(e).__name__}: {e}"


def query_aggregate(credit, dim):
    body = {"creditCode": credit, "dataType": dim}
    payload = {"encryptionType": 1, "appKey": APP_KEY, "sign": sign(body, SECRET), "body": body}
    return http_post(BASE + QPATH, payload)


def fetch_served(request_id):
    """下钻 upstream_call，返回 (served_provider, ok_detail, all_calls)。
    served_provider = status=ok 那一源的 provider；查无时为 None。轮询等待异步落库。"""
    for _ in range(10):
        st, obj, err = admin_get(f"/admin/api/{VER}/audits/{request_id}/calls")
        calls = (obj or {}).get("calls") if isinstance(obj, dict) else None
        if calls:
            ok = [c for c in calls if c.get("status") == "ok"]
            served = ok[0].get("provider") if ok else None
            detail = [{"source": c.get("source"), "provider": c.get("provider"),
                       "status": c.get("status"), "code": c.get("code"),
                       "reason": c.get("reason")} for c in calls]
            return served, (ok[0] if ok else None), detail
        time.sleep(1.5)
    return "__unknown__", None, []


# ---------------- 任务 ----------------
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
                done[r["qkey"]] = r
    return done


def run_one(task):
    """task: {qkey(dim|credit), credit, dim}. 重调聚合 + 下钻实际查得上游。"""
    credit, dim = task["credit"], task["dim"]
    qkey = task["qkey"]
    status, raw, err = query_aggregate(credit, dim)
    if err or status >= 500 or status == 0:
        time.sleep(1.5)
        status, raw, err = query_aggregate(credit, dim)
    rec = {"qkey": qkey, "credit": credit, "dim": dim, "httpStatus": status,
           "error": err, "errorCode": "", "errorMsg": "", "logId": "",
           "bodyCode": "", "bodyMsg": "", "reqid": "", "served": None,
           "servedCode": "", "calls": [], "outcome": "失败"}
    if raw:
        (RAW / f"relay_{dim}_{credit}.json").write_text(raw, encoding="utf-8")
    if err or not raw:
        rec["errorMsg"] = err or "empty body"
        append_progress(rec)
        return rec
    try:
        obj = json.loads(raw)
    except json.JSONDecodeError:
        rec["errorMsg"] = raw[:200]
        append_progress(rec)
        return rec
    head, body = obj.get("head") or {}, obj.get("body") or {}
    rec["errorCode"] = str(head.get("errorCode") or "")
    rec["errorMsg"] = str(head.get("errorMsg") or "")
    rec["logId"] = str(head.get("logId") or "")
    rec["bodyCode"] = str(body.get("code") or "")
    rec["bodyMsg"] = str(body.get("msg") or "")
    rec["reqid"] = str(body.get("reqid") or "")
    if rec["errorCode"] != "0":
        append_progress(rec)
        return rec
    # 下钻实际查得上游
    if rec["logId"]:
        served, ok, calls = fetch_served(rec["logId"])
        rec["served"] = served
        rec["servedCode"] = str((ok or {}).get("code") or "")
        rec["calls"] = calls
    if rec["bodyCode"] == "001":
        rec["outcome"] = "查得"
    elif rec["bodyCode"] == "002":
        rec["outcome"] = "未取得"
    else:
        rec["outcome"] = "查无"
    append_progress(rec)
    return rec


def load_tasks():
    tasks_list = json.loads((OUT / "tasks.json").read_text(encoding="utf-8"))
    # 按 (dim, credit) 去重（聚合查询只依赖 credit+dim，与上游无关）；只保留目标上游涉及的。
    uniq = {}
    for t in tasks_list:
        if t["upstream"] not in UPSTREAMS:
            continue
        qkey = f"{t['dim']}|{t['credit']}"
        if qkey not in uniq:
            uniq[qkey] = {"qkey": qkey, "credit": t["credit"], "dim": t["dim"]}
    return list(uniq.values())


# ---------------- Excel 输出 ----------------
HEADERS = [
    "税号", "企业名称", "类目", "查得子源(账单)", "本次调用结果", "实际查得上游", "归属一致",
    "近24月开票金额", "发票为0", "近24月应税销售收入", "近24月应纳税额", "近24月实缴金额",
    "近24月税务金额", "税务为0", "近24月总金额", "总金额<=0", "说明",
    "窗口", "httpStatus", "errorCode", "bodyCode", "bodyMsg", "reqid", "logId", "逐源明细",
]
WIDTHS = [22, 30, 6, 12, 10, 12, 8, 16, 9, 20, 18, 18, 16, 9, 16, 11, 40,
          16, 10, 10, 9, 14, 20, 34, 50]


def money(v):
    return "" if v in (None, "") else float(Decimal(str(v)))


def yn(v):
    return "" if v is None else ("是" if v else "否")


def build_row(file_up, row, rec, frm, to):
    """返回 (excel_row, is_zero_for_this_upstream)。"""
    credit, cat, dim, sub = row["credit"], row["cat"], row["dim"], row.get("sub", "")
    served = rec.get("served") if rec else None
    served_cn = UP_CN.get(served, served or "")
    calls_s = "; ".join(f"{c['source']}:{c['status']}" for c in (rec or {}).get("calls", []))
    base = [credit, "", cat, sub, (rec or {}).get("outcome", "未重调"), served_cn, "", "",
            "", "", "", "", "", "", "", "", "", f"{frm}-{to}",
            (rec or {}).get("httpStatus", ""), (rec or {}).get("errorCode", ""),
            (rec or {}).get("bodyCode", ""), (rec or {}).get("bodyMsg", ""),
            (rec or {}).get("reqid", ""), (rec or {}).get("logId", ""), calls_s]

    if not rec or rec.get("outcome") == "失败":
        base[16] = "重调失败，需人工：" + (rec or {}).get("errorMsg", "无记录")
        return base, False
    if rec["outcome"] in ("查无", "未取得"):
        base[16] = f"重调{rec['outcome']}，需人工复核（历史由{UP_CN.get(file_up)}查得）"
        return base, False
    # 查得：校验实际查得上游
    if served == "__unknown__":
        base[6] = "未知"
        base[16] = "已查得但后台逐源明细暂不可取，归属待核，需人工"
        return base, False
    if served != file_up:
        base[6] = "否"
        base[16] = (f"本次由{served_cn}查得（非{UP_CN.get(file_up)}），被更高优先级上游拦截；"
                    f"无法据此判定{UP_CN.get(file_up)}金额，需直连{UP_CN.get(file_up)}核对")
        return base, False
    # 归属一致：计算金额
    base[6] = "是"
    raw_path = RAW / f"relay_{dim}_{credit}.json"
    if not raw_path.exists():
        base[16] = "原始响应缺失，需重调"
        return base, False
    obj = json.loads(raw_path.read_text(encoding="utf-8"))
    rng = ((obj.get("body") or {}).get("result") or {}).get("range")
    if isinstance(rng, str):
        rng = json.loads(rng)
    j = judge_amounts(rng, dim, frm, to)
    base[1] = j["company"]
    if dim in ("invoice", "both"):
        base[7], base[8] = money(j["inv_amt"]), yn(j["inv_zero"])
    if dim in ("tax", "both"):
        base[9], base[10], base[11] = (money(x) for x in j["tax_parts"])
        base[12], base[13] = money(j["tax_amt"]), yn(j["tax_zero"])
    base[14], base[15] = money(j["total"]), yn(j["is_zero"])
    if j["is_zero"]:
        base[16] = f"近24月总金额<=0，不需向{UP_CN.get(file_up)}缴费"
    else:
        base[16] = f"近24月总金额>0，需向{UP_CN.get(file_up)}缴费"
    return base, j["is_zero"]


def write_file(meta, cache):
    up, cn = meta["upstream"], meta["cn"]
    frm, to = meta["from"], meta["to"]
    src, out = Path(meta["src"]), Path(meta["out"])
    wb = load_workbook(src)
    for s in ("24月重调明细", "24月不需缴费"):
        if s in wb.sheetnames:
            del wb[s]
    header_font = Font(bold=True, color="FFFFFF")
    header_fill = PatternFill("solid", fgColor="1F4E79")
    zero_fill = PatternFill("solid", fgColor="C6EFCE")

    detail, zero = [], []
    for row in meta["rows"]:
        rec = cache.get(f"{row['dim']}|{row['credit']}")
        r, is_zero = build_row(up, row, rec, frm, to)
        detail.append((r, is_zero))
        if is_zero:
            zero.append((r, True))

    def add_sheet(title, rows, note):
        ws = wb.create_sheet(title)
        ws.append([note] + [""] * (len(HEADERS) - 1))
        ws.append(HEADERS)
        for cell in ws[2]:
            cell.font, cell.fill = header_font, header_fill
            cell.alignment = Alignment(horizontal="center")
        for r, is_zero in rows:
            ws.append(r)
            if is_zero:
                for cell in ws[ws.max_row]:
                    cell.fill = zero_fill
        for i, w in enumerate(WIDTHS, 1):
            ws.column_dimensions[get_column_letter(i)].width = w
        ws.freeze_panes = "A3"
        ws.auto_filter.ref = f"A2:{get_column_letter(len(HEADERS))}{ws.max_row}"

    note_all = (f"对账单「查得」税号重调 swfp 聚合接口（窗口 {frm}-{to}，账单月及之前24个月，"
                f"北京时间）。每次重调经后台 upstream_call 下钻，取 status=ok 源 provider 为"
                f"「实际查得上游」，只有=={cn}时才据此判金额，否则标注需人工/直连核对——"
                f"保证返回是{cn}的数据。")
    note_zero = (f"以下税号实际由{cn}查得且近24月总金额<=0，不需向{cn}缴费（已高亮）。")
    add_sheet("24月重调明细", detail, note_all)
    add_sheet("24月不需缴费", [z for z in zero], note_zero)
    wb.save(out)
    print(f"[{up}_{meta['month']}] {out.name}: 明细 {len(detail)} 行，不需缴费 {len(zero)} 行")
    return len(detail), len(zero)


def main():
    mode = sys.argv[1] if len(sys.argv) > 1 else "run"
    OUT.mkdir(parents=True, exist_ok=True)
    RAW.mkdir(parents=True, exist_ok=True)

    if mode in ("run", "all"):
        tasks = load_tasks()
        done = load_done()
        pending = [t for t in tasks
                   if not (done.get(t["qkey"]) and done[t["qkey"]].get("outcome") in
                           ("查得", "查无", "未取得"))]
        print(f"唯一聚合查询 {len(tasks)} 个（证通/惠众/税友去重后），已完成 "
              f"{len(tasks) - len(pending)}，待调 {len(pending)}；base={BASE} workers={WORKERS}")
        if pending:
            with ThreadPoolExecutor(max_workers=WORKERS) as pool:
                futs = {pool.submit(run_one, t): t for t in pending}
                n = 0
                for fut in as_completed(futs):
                    n += 1
                    try:
                        rec = fut.result()
                    except Exception as ex:
                        rec = {"credit": "?", "outcome": "失败", "error": str(ex)}
                    print(f"[{n}/{len(pending)}] {rec.get('credit')} {rec.get('dim')} "
                          f"{rec.get('outcome')} served={rec.get('served')} "
                          f"code={rec.get('bodyCode')} {rec.get('errorMsg') or ''}", flush=True)

    if mode in ("write", "all", "run"):
        files_meta = json.loads((OUT / "files.json").read_text(encoding="utf-8"))
        cache = load_done()
        totals = []
        for fkey, meta in files_meta.items():
            if meta["upstream"] not in UPSTREAMS:
                continue
            totals.append(write_file(meta, cache))
        print(f"\n完成。6 个文件合计 不需缴费行 = {sum(z for _, z in totals)}")


if __name__ == "__main__":
    main()
