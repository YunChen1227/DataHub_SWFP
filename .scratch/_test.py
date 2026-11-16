import hashlib, json, urllib.request, urllib.error
APP="j92eme33uipj"; SEC="4c08fd3cafb7615e99506ae3e5d694f4"
def sign(body):
    parts=[f"{k}{body[k]}" for k in sorted(body) if body[k] not in (None,"")]
    return hashlib.md5(("".join(parts)+SEC).encode()).hexdigest()
def call(base, credit, dt):
    body={"creditCode":credit,"dataType":dt}
    payload={"encryptionType":1,"appKey":APP,"sign":sign(body),"body":body}
    req=urllib.request.Request(base+"/v1/openapi/zlx/querySrmxSWFP",
        data=json.dumps(payload,ensure_ascii=False).encode(),
        headers={"Content-Type":"application/json; charset=utf-8"},method="POST")
    try:
        with urllib.request.urlopen(req,timeout=30) as r:
            return r.status, r.read().decode("utf-8","replace")
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8","replace")
    except Exception as e:
        return 0, f"{type(e).__name__}: {e}"
for base in ["http://www.aiszcloud.cn:8080","http://www.aiszcloud.cn:8081"]:
    s,raw=call(base,"91440300MA5F0M1Y40","both")
    print("BASE",base,"status",s)
    print(raw[:600])
    print("-"*50)
