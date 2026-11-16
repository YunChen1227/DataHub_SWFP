# -*- coding: utf-8 -*-
import hashlib, json, os, sys, time, urllib.request, urllib.error
sys.stdout.reconfigure(encoding="utf-8")
APP_KEY = os.environ["SWFP_APP_KEY"]; SECRET = os.environ["SWFP_SECRET"]
BASE = os.environ.get("RELAY_BASE_URL", "http://www.aiszcloud.cn:8081")
PATH = "/v1/openapi/zlx/querySrmxSWFP"

def sign(body):
    parts = [f"{k}{body[k]}" for k in sorted(body) if body[k] not in (None, "")]
    return hashlib.md5(("".join(parts) + SECRET).encode()).hexdigest()

def call(cc, dt):
    body = {"creditCode": cc, "dataType": dt}
    payload = {"encryptionType": 1, "appKey": APP_KEY, "sign": sign(body), "body": body}
    data = json.dumps(payload, ensure_ascii=False).encode()
    req = urllib.request.Request(BASE + PATH, data=data,
        headers={"Content-Type": "application/json; charset=utf-8"}, method="POST")
    t0 = time.perf_counter()
    try:
        with urllib.request.urlopen(req, timeout=40) as r:
            raw = r.read().decode("utf-8", "replace"); st = r.status
    except urllib.error.HTTPError as e:
        raw = e.read().decode("utf-8", "replace"); st = e.code
    dt_s = time.perf_counter() - t0
    obj = json.loads(raw)
    head, body2 = obj.get("head") or {}, obj.get("body") or {}
    scope = ""
    if str(body2.get("code")) == "001":
        rng = (body2.get("result") or {}).get("range")
        if isinstance(rng, str):
            rng = json.loads(rng)
        scope = json.dumps((rng or {}).get("dataScope") or {}, ensure_ascii=False)
    print(f"{cc} dt={dt} http={st} {dt_s:.1f}s errCode={head.get('errorCode')} "
          f"msg={head.get('errorMsg')} code={body2.get('code')} bytes={len(raw)} scope={scope}")

# Oct invoice-found, Sept tax-found, and a both
call("92340400MA2PHGLM3C", "invoice")
call("91110106596038797J", "tax")
