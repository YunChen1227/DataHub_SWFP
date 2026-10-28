# -*- coding: utf-8 -*-
import json
import sys
from pathlib import Path

sys.stdout.reconfigure(encoding="utf-8")
raw_dir = Path(r"c:\workspace\DataHub_SWFP\test_res\北京联信-各上游最新查得-raw")
for p in sorted(raw_dir.glob("*.json")):
    obj = json.loads(p.read_text(encoding="utf-8"))
    head = obj.get("head") or {}
    body = obj.get("body") or {}
    result = body.get("result") or {}
    rng = result.get("range")
    scope = None
    inv_keys = tax_keys = None
    if isinstance(rng, str) and rng:
        try:
            parsed = json.loads(rng)
        except json.JSONDecodeError:
            parsed = {}
        scope = parsed.get("dataScope")
        inv = parsed.get("发票数据聚合") or {}
        tax = parsed.get("税务数据聚合") or {}
        inv_keys = list(inv)[:8] if isinstance(inv, dict) else type(inv).__name__
        tax_keys = list(tax)[:8] if isinstance(tax, dict) else type(tax).__name__
    print(p.name)
    print("  errorCode", head.get("errorCode"), "errorMsg", head.get("errorMsg"), "time", head.get("time"))
    print("  body", body.get("code"), body.get("msg"), "reqid", body.get("reqid"))
    print("  dataScope", scope)
    print("  invoice_keys", inv_keys)
    print("  tax_keys", tax_keys)
    print("  bytes", p.stat().st_size)
