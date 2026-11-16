# -*- coding: utf-8 -*-
import base64, hashlib, hmac, json, time, urllib.request, urllib.error, sys
sys.stdout.reconfigure(encoding="utf-8")

BASE = "http://www.aiszcloud.cn:8081"
APP_KEY = "j92eme33uipj"
SECRET = "edd726edad2da85f39285e5e81f21af5"
JWT_SECRET = "aishuzhiadminjwtsecret"
VER = "swfp"


def b64u(b):
    return base64.urlsafe_b64encode(b).decode().rstrip("=")


def mint_jwt():
    now = int(time.time())
    h = b64u(json.dumps({"alg": "HS256", "typ": "JWT"}, separators=(",", ":")).encode())
    c = b64u(json.dumps({"sub": "admin", "exp": now + 3600, "iat": now}, separators=(",", ":")).encode())
    inp = f"{h}.{c}"
    sig = b64u(hmac.new(JWT_SECRET.encode(), inp.encode(), hashlib.sha256).digest())
    return f"{inp}.{sig}"


def sign_body(body):
    parts = [f"{k}{body[k]}" for k in sorted(body) if body[k] not in (None, "")]
    return hashlib.md5(("".join(parts) + SECRET).encode()).hexdigest()


def query(credit, dt):
    body = {"creditCode": credit, "dataType": dt}
    payload = {"encryptionType": 1, "appKey": APP_KEY, "sign": sign_body(body), "body": body}
    req = urllib.request.Request(BASE + "/v1/openapi/zlx/querySrmxSWFP",
        data=json.dumps(payload).encode(), headers={"Content-Type": "application/json"}, method="POST")
    with urllib.request.urlopen(req, timeout=40) as r:
        return json.loads(r.read().decode())


def admin_get(path, token):
    req = urllib.request.Request(BASE + path, headers={"Authorization": "Bearer " + token})
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return r.status, json.loads(r.read().decode())
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()[:300]


def main():
    token = mint_jwt()
    print("JWT:", token[:40], "...")
    st, body = admin_get(f"/admin/api/{VER}/audits?limit=1", token)
    print("admin audits probe:", st, json.dumps(body, ensure_ascii=False)[:200] if isinstance(body, dict) else body)

    samples = [("91341600MA8P831P7A", "tax", "证通"),
               ("91370212MA3MK6PE1R", "tax", "惠众"),
               ("91440606MA55UMTC72", "tax", "税友")]
    for credit, dt, expect in samples:
        resp = query(credit, dt)
        head = resp.get("head", {})
        b = resp.get("body", {})
        logid = head.get("logId", "")
        print(f"\n== {credit} dt={dt} 期望上游={expect} ==")
        print(f"  head.errorCode={head.get('errorCode')} body.code={b.get('code')} logId={logid}")
        # wait for async bookkeeping then fetch calls
        found = None
        for _ in range(8):
            time.sleep(1.5)
            st, calls = admin_get(f"/admin/api/{VER}/audits/{logid}/calls", token)
            if isinstance(calls, dict) and calls.get("calls"):
                found = calls
                break
        if found:
            for c in found["calls"]:
                print(f"    source={c.get('source')} label={c.get('label')} provider={c.get('provider')} "
                      f"status={c.get('status')} code={c.get('code')} dims={c.get('dims')} reason={c.get('reason','')}")
        else:
            print("    calls 未取到:", st, str(calls)[:200])


if __name__ == "__main__":
    main()
