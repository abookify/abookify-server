#!/usr/bin/env python3
"""Endpoint status-contract smoke suite for the abookify server.

Complements api_smoke.py (which checks response *content*). This suite hits
each MAJOR ROUTE against a running server and asserts the status CONTRACT:

  * a 2xx, OR
  * a graceful 4xx/503 whose body is JSON carrying an "error" string,
  * but NEVER a bare 500 (or any non-503 5xx).

It exists to catch the class of bug where a foreseeable condition (an opt-in
service not running, a missing alignment, no LLM key) surfaces as a raw
"internal server error" 500 instead of an actionable message. In particular it
locks the cast-extraction graceful-degradation contract: with the opt-in
BookNLP service NOT running, POST extract-cast must return a 503 (or 403/422),
never a 500.

Usage: endpoint_smoke.py [base-url]
Env:   ABOOKIFY_TOKEN  optional bearer token (for a server with auth enabled).
Exit:  non-zero if any route violates the contract.
"""

from __future__ import annotations
import json
import os
import sys
import urllib.error
import urllib.request

# Status the contract permits. 2xx pass outright; these 4xx/503 are graceful
# IFF the body is a JSON object with an "error". Anything else (esp. 500/502/
# 504) is a contract violation.
GRACEFUL = {400, 401, 403, 404, 405, 409, 415, 422, 429, 503}

TOKEN = os.environ.get("ABOOKIFY_TOKEN", "").strip()


class Result:
    def __init__(self) -> None:
        self.passed = 0
        self.failed = 0


def request(base: str, method: str, path: str, body: dict | None = None):
    """Return (status, parsed_json_or_text, raw_text)."""
    url = base + path
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    if data is not None:
        req.add_header("Content-Type", "application/json")
    if TOKEN:
        req.add_header("Authorization", "Bearer " + TOKEN)
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            raw = r.read().decode("utf-8", "replace")
            return r.status, _try_json(raw), raw
    except urllib.error.HTTPError as e:
        raw = e.read().decode("utf-8", "replace")
        return e.code, _try_json(raw), raw
    except Exception as e:
        return None, None, str(e)


def _try_json(raw: str):
    try:
        return json.loads(raw)
    except Exception:
        return None


def check(res: Result, base: str, method: str, path: str, *,
          body: dict | None = None, expect: set[int] | None = None,
          note: str = "") -> int | None:
    """Assert the status contract for one route. Returns the status code."""
    status, parsed, raw = request(base, method, path, body)
    label = f"{method} {path}" + (f"  ({note})" if note else "")

    if status is None:
        res.failed += 1
        print(f"✗ {label}\n    transport error: {raw[:160]}")
        return None

    ok = False
    why = ""
    if expect is not None:
        ok = status in expect
        why = f"want one of {sorted(expect)}"
    elif 200 <= status < 300:
        ok = True
    elif status in GRACEFUL:
        # Graceful only if it's a JSON object with an "error" message.
        ok = isinstance(parsed, dict) and isinstance(parsed.get("error"), str) and parsed["error"] != ""
        why = "graceful 4xx/503 must carry a JSON {\"error\": ...}"
    else:
        why = "bare 5xx / non-graceful status"

    if ok:
        res.passed += 1
        print(f"✓ {label} → {status}")
    else:
        res.failed += 1
        err = (parsed or {}).get("error") if isinstance(parsed, dict) else None
        print(f"✗ {label} → {status}  [{why}]\n    body: {(err or raw)[:160]}")
    return status


def discover(base: str):
    """Find sample ids from the live library; tolerate an empty library."""
    status, works, _ = request(base, "GET", "/api/works")
    if not isinstance(works, list) or not works:
        return {}
    pick = {"work": works[0]["id"]}
    for w in works:
        if w.get("audio_files"):
            pick.setdefault("audio_work", w["id"])
            pick.setdefault("audio_book", w["audio_files"][0]["id"])
        tfs = w.get("text_files") or []
        if tfs:
            pick.setdefault("text_work", w["id"])
            pick.setdefault("text_book", tfs[0]["id"])
        for tf in tfs:
            if tf.get("format") == "epub":
                pick.setdefault("epub_work", w["id"])
    return pick


def main() -> int:
    base = (sys.argv[1] if len(sys.argv) > 1 else "http://localhost:7654").rstrip("/")
    res = Result()
    print(f"→ endpoint smoke against {base}"
          + (" (authenticated)" if TOKEN else "") + "\n")

    # Always-available, unauthenticated routes.
    check(res, base, "GET", "/api/health")
    check(res, base, "GET", "/api/ready", expect={200, 503})
    check(res, base, "GET", "/api/info")
    check(res, base, "GET", "/api/setup")
    check(res, base, "GET", "/api/settings/schema")

    # Authenticated/config routes (graceful 401 when auth is on + no token).
    check(res, base, "GET", "/api/works")
    check(res, base, "GET", "/api/catalog")
    check(res, base, "GET", "/api/settings")
    check(res, base, "GET", "/api/logs")

    ids = discover(base)
    if not ids:
        print("\n  (library empty or unauthorized — per-work routes skipped)")
    else:
        w = ids.get("work")
        check(res, base, "GET", f"/api/works/{w}/coverage", note="empty ok")
        check(res, base, "GET", f"/api/works/{w}/diff", note="404 ok when unaligned")
        check(res, base, "GET", f"/api/works/{w}/cast")
        check(res, base, "GET", f"/api/works/{w}/qa-suggestions")
        if "audio_work" in ids:
            check(res, base, "GET", f"/api/works/{ids['audio_work']}/waveform")
            check(res, base, "GET", f"/api/books/{ids['audio_book']}/waveform")
        if "text_work" in ids:
            tw, tb = ids["text_work"], ids["text_book"]
            check(res, base, "GET", f"/api/works/{tw}/text-sync/{tb}/0", note="mode none/paragraph")
            check(res, base, "GET", f"/api/works/{tw}/word-sync/{tb}/0")
            check(res, base, "GET", f"/api/books/{tb}/chapters")

    # --- The cast graceful-degradation contract (PJ's bug) -----------------
    # With BookNLP not running, POST extract-cast must be graceful (503 ideal;
    # 403 when the flag is off; 422 when the work has no EPUB) — NEVER a 500.
    cast_work = ids.get("epub_work") or ids.get("work")
    if cast_work is not None:
        # Best-effort enable so we exercise the service-down 503 path (works on
        # an open server / with a token; silently no-ops behind auth).
        request(base, "POST", "/api/settings", {"booknlp_enabled": "true"})
        check(res, base, "POST", f"/api/works/{cast_work}/extract-cast",
              expect={403, 422, 503}, note="must fail soft when BookNLP is down")
    else:
        print("  (no work to test cast extraction)")

    print(f"\n=== {res.passed} passed, {res.failed} failed ===")
    return 1 if res.failed else 0


if __name__ == "__main__":
    sys.exit(main())
