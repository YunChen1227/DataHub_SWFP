# -*- coding: utf-8 -*-
"""One-off SWFP lookup. Reads SWFP_APP_KEY / SWFP_SECRET from the environment."""
import hashlib
import json
import os
import sys
import time
import urllib.error
import urllib.request
from collections import defaultdict
from decimal import Decimal
from pathlib import Path

sys.stdout.reconfigure(encoding="utf-8")

APP_KEY = os.environ["SWFP_APP_KEY"]
SECRET = os.environ["SWFP_SECRET"]
BASE = "http://www.aiszcloud.cn:8081"
PATH = "/v1/openapi/zlx/querySrmxSWFP"
CODE = sys.argv[1]
OUT = Path(sys.argv[2])


def sign(body: dict) -> str:
    parts = []
    for k in sorted(body.keys()):
        v = body[k]
        if v is None or v == "":
            continue
        parts.append(f"{k}{v}")
    return hashlib.md5(("".join(parts) + SECRET).encode("utf-8")).hexdigest()


def query(data_type: str):
    body = {"creditCode": CODE, "dataType": data_type}
    payload = {
        "encryptionType": 1,
        "appKey": APP_KEY,
        "sign": sign(body),
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
        with urllib.request.urlopen(req, timeout=40) as resp:
            raw = resp.read().decode("utf-8", errors="replace")
            return resp.status, raw, time.perf_counter() - t0
    except urllib.error.HTTPError as e:
        raw = e.read().decode("utf-8", errors="replace")
        return e.code, raw, time.perf_counter() - t0


def D(v):
    try:
        return Decimal(str(v or "0"))
    except Exception:
        return Decimal(0)


def main():
    status, raw, elapsed = query("both")
    obj = json.loads(raw)
    body = obj.get("body") or {}
    print(f"try1 http={status} elapsed={elapsed:.2f}s bytes={len(raw.encode('utf-8'))} body.code={body.get('code')} msg={body.get('msg')}")
    if str(body.get("code")) == "002":
        time.sleep(1.5)
        status, raw, elapsed = query("both")
        obj = json.loads(raw)
        body = obj.get("body") or {}
        print(f"try2 http={status} elapsed={elapsed:.2f}s bytes={len(raw.encode('utf-8'))} body.code={body.get('code')} msg={body.get('msg')}")
    OUT.write_text(raw, encoding="utf-8")
    head = obj.get("head") or {}
    print(f"head.errorCode={head.get('errorCode')} errorMsg={head.get('errorMsg')} logId={head.get('logId')}")
    if str(body.get("code")) != "001":
        return
    result = body.get("result") or {}
    rng = result.get("range")
    if isinstance(rng, str):
        rng = json.loads(rng)
    scope = (rng or {}).get("dataScope") or {}
    print("dataScope", json.dumps(scope, ensure_ascii=False))
    inv = (rng or {}).get("发票数据聚合") or {}
    tax = (rng or {}).get("税务数据聚合") or {}
    nsr = inv.get("nsrjbxx") or {}
    print("NSR", json.dumps(nsr, ensure_ascii=False))
    kp = inv.get("kphzxxList") or []
    print(f"kphz rows={len(kp)}")
    if kp:
        amt = sum(D(r.get("kpje")) for r in kp)
        taxamt = sum(D(r.get("ljse")) for r in kp)
        cnt = sum(int(D(r.get("ljkpcs"))) for r in kp)
        red = sum(D(r.get("hpje")) for r in kp)
        print(f"kpje_sum={amt} ljse_sum={taxamt} ljkpcs={cnt} hpje_sum={red} span={kp[0].get('ssyf')}..{kp[-1].get('ssyf')}")
        top = max(kp, key=lambda r: D(r.get("kpje")))
        print(f"peak {top.get('ssyf')} kpje={top.get('kpje')} max_ticket={top.get('dzzgkpjejlp')}")
    xy = inv.get("xyhzxxList") or []
    sy = inv.get("syhzxxList") or []
    sp = inv.get("spxxList") or []
    kh = inv.get("khxsdqList") or []
    print(f"lists xy={len(xy)} sy={len(sy)} sp={len(sp)} kh={len(kh)}")

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
    print("---DOWN---")
    for name, rec in sorted(down.items(), key=lambda x: -x[1]["amt"])[:25]:
        months = sorted(x for x in rec["months"] if x)
        span = f"{months[0]}..{months[-1]}" if months else ""
        print(f"{rec['amt']}\t{rec['cnt']}\t{len(months)}\t{span}\t{rec['taxno']}\t{name}")
    print("---UP---")
    for name, rec in sorted(up.items(), key=lambda x: -x[1]["amt"])[:20]:
        months = sorted(x for x in rec["months"] if x)
        span = f"{months[0]}..{months[-1]}" if months else ""
        print(f"{rec['amt']}\t{rec['cnt']}\t{len(months)}\t{span}\t{rec['taxno']}\t{name}")
    goods = defaultdict(lambda: Decimal(0))
    for r in sp:
        goods[r.get("hwhlwmc") or ""] += D(r.get("spzje"))
    print("---GOODS---")
    for name, amt in sorted(goods.items(), key=lambda x: -x[1])[:15]:
        print(f"{amt}\t{name}")
    tax_nsr = tax.get("nsrjbxx") or {}
    print("TAX_NSR", json.dumps(tax_nsr, ensure_ascii=False)[:800])
    for k in ("lrbxxList", "sbsjList", "zcfzbxxList", "zsbxxList"):
        rows = tax.get(k) or []
        print(f"tax {k} rows={len(rows)}")


if __name__ == "__main__":
    main()
