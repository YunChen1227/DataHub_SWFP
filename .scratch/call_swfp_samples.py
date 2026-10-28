# -*- coding: utf-8 -*-
"""Call Aliyun SWFP for each row: 查询票 -> dataType=invoice, 查询税 -> dataType=tax."""
import csv
import hashlib
import json
import os
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

sys.stdout.reconfigure(encoding="utf-8")

SRC = Path(r"c:\Users\XMMXYY\Downloads\北京联信-各上游最新查得.csv")
OUT_DIR = Path(r"c:\workspace\DataHub_SWFP\test_res")
OUT_CSV = OUT_DIR / "北京联信-各上游最新查得-接口返回.csv"
OUT_XLSX = OUT_DIR / "北京联信-各上游最新查得-接口返回.xlsx"
RAW_DIR = OUT_DIR / "北京联信-各上游最新查得-raw"

APP_KEY = os.environ["SWFP_APP_KEY"]
SECRET = os.environ["SWFP_SECRET"]
BASES = [
    os.environ.get("RELAY_BASE_URL", "").rstrip("/"),
    "http://www.aiszcloud.cn:8081",
    "http://aiszcloud.cn:8081",
]
BASES = [b for b in BASES if b]
PATH = "/v1/openapi/zlx/querySrmxSWFP"

TYPE_MAP = {"查询票": "invoice", "查询税": "tax"}


def sign(body: dict, secret: str) -> str:
    parts = []
    for k in sorted(body.keys()):
        v = body[k]
        if v is None or v == "":
            continue
        parts.append(f"{k}{v}")
    raw = "".join(parts) + secret
    return hashlib.md5(raw.encode("utf-8")).hexdigest()


def query(base: str, credit_code: str, data_type: str) -> tuple[int, str, float]:
    body = {"creditCode": credit_code, "dataType": data_type}
    payload = {
        "encryptionType": 1,
        "appKey": APP_KEY,
        "sign": sign(body, SECRET),
        "body": body,
    }
    data = json.dumps(payload, ensure_ascii=False).encode("utf-8")
    req = urllib.request.Request(
        base + PATH,
        data=data,
        headers={"Content-Type": "application/json; charset=utf-8"},
        method="POST",
    )
    t0 = time.perf_counter()
    try:
        with urllib.request.urlopen(req, timeout=25) as resp:
            raw = resp.read().decode("utf-8", errors="replace")
            return resp.status, raw, time.perf_counter() - t0
    except urllib.error.HTTPError as e:
        raw = e.read().decode("utf-8", errors="replace")
        return e.code, raw, time.perf_counter() - t0


def summarize(raw: str) -> str:
    try:
        obj = json.loads(raw)
    except json.JSONDecodeError:
        return raw[:200]
    head = obj.get("head") or {}
    body = obj.get("body") or {}
    return (
        f"http_ok errorCode={head.get('errorCode')} msg={head.get('errorMsg')} "
        f"body.code={body.get('code')} body.msg={body.get('msg')} "
        f"bytes={len(raw.encode('utf-8'))}"
    )


def main() -> None:
    OUT_DIR.mkdir(parents=True, exist_ok=True)
    RAW_DIR.mkdir(parents=True, exist_ok=True)
    with SRC.open("r", encoding="utf-8-sig", newline="") as f:
        rows = list(csv.DictReader(f))
    if not rows:
        raise SystemExit("empty csv")

    # Probe the first reachable base with the first row, then reuse it.
    base = None
    first = rows[0]
    data_type = TYPE_MAP[first["查询类型"].strip()]
    last_err = ""
    for candidate in BASES:
        try:
            status, raw, elapsed = query(candidate, first["税号"].strip(), data_type)
        except Exception as e:
            last_err = f"{candidate}: {type(e).__name__}: {e}"
            print("FAIL", last_err)
            continue
        print(f"PROBE {candidate} status={status} {elapsed:.2f}s {summarize(raw)}")
        try:
            err = str((json.loads(raw).get("head") or {}).get("errorCode"))
        except json.JSONDecodeError:
            err = ""
        if status == 200 and err not in {"505004", "505001"}:
            base = candidate
            probe = (status, raw, elapsed)
            break
        last_err = f"{candidate} status={status} {summarize(raw)}"
    if base is None:
        raise SystemExit("no usable endpoint: " + last_err)

    results = []
    for i, row in enumerate(rows):
        credit = row["税号"].strip()
        kind = row["查询类型"].strip()
        data_type = TYPE_MAP[kind]
        if i == 0:
            status, raw, elapsed = probe
        else:
            try:
                status, raw, elapsed = query(base, credit, data_type)
            except Exception as e:
                status, raw, elapsed = 0, json.dumps({"error": f"{type(e).__name__}: {e}"}, ensure_ascii=False), 0
        print(f"[{i+1}/{len(rows)}] {credit} {kind} dataType={data_type} {elapsed:.2f}s {summarize(raw)}")
        safe = f"{i+1:02d}_{data_type}_{credit}.json"
        (RAW_DIR / safe).write_text(raw, encoding="utf-8")
        out = dict(row)
        out["dataType"] = data_type
        out["httpStatus"] = status
        out["耗时秒"] = f"{elapsed:.2f}"
        out["接口返回"] = raw
        results.append(out)

    fields = list(rows[0].keys()) + ["dataType", "httpStatus", "耗时秒", "接口返回"]
    with OUT_CSV.open("w", encoding="utf-8-sig", newline="") as f:
        w = csv.DictWriter(f, fieldnames=fields)
        w.writeheader()
        w.writerows(results)

    try:
        from openpyxl import Workbook

        wb = Workbook()
        ws = wb.active
        ws.title = "查得样本"
        ws.append(fields)
        too_long = False
        for rec in results:
            values = []
            for k in fields:
                v = rec.get(k, "")
                if k == "接口返回" and isinstance(v, str) and len(v) > 32000:
                    too_long = True
                    v = v[:32000]
                values.append(v)
            ws.append(values)
        wb.save(OUT_XLSX)
        print("xlsx", OUT_XLSX, "truncated" if too_long else "full")
    except Exception as e:
        print("xlsx skip", type(e).__name__, e)

    print("csv", OUT_CSV)
    print("raw", RAW_DIR)
    print("base", base)


if __name__ == "__main__":
    main()
