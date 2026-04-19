#!/usr/bin/env python3
"""Structured API smoke test for the abookify server.

Verifies that every work in the library can be fully exercised through the
same API calls the web UI makes. Catches classes of bugs the user has caught
(non-array chapter responses, zero-filled sync data, empty chapter content)
before they reach the UI.

Not a unit test — this runs against a real running server with real library
data, so it's a functional smoke test.
"""

from __future__ import annotations
import json
import sys
import urllib.error
import urllib.request


def fetch(url: str) -> object:
    try:
        with urllib.request.urlopen(url, timeout=10) as r:
            return json.load(r)
    except urllib.error.HTTPError as e:
        return {"_error": f"HTTP {e.code}", "_body": e.read()[:200].decode("utf-8", "replace")}
    except Exception as e:
        return {"_error": str(e)}


def check_work(base: str, w: dict) -> list[str]:
    """Return a list of failure strings. Empty list = pass."""
    failures: list[str] = []
    label = f"#{w['id']} {w['title'][:55]}"

    # --- Text-pane checks ---------------------------------------------------
    for tf in (w.get("text_files") or []):
        book_id = tf["id"]
        body = fetch(f"{base}/api/books/{book_id}/chapters")

        if not isinstance(body, list):
            failures.append(
                f"book {book_id}: /chapters returned non-array: "
                f"{str(body)[:120]}"
            )
            continue

        if len(body) == 0:
            # Not a failure — some text files may genuinely have no chapters.
            continue

        # Sample the first chapter's content endpoint.
        ch0 = fetch(f"{base}/api/books/{book_id}/chapters/0")
        if not isinstance(ch0, dict):
            failures.append(f"book {book_id}: /chapters/0 returned non-object: {str(ch0)[:120]}")
            continue

        wc = ch0.get("word_count", 0)
        clen = len(ch0.get("content") or "") + len(ch0.get("content_html") or "")
        if wc > 0 and clen == 0:
            failures.append(
                f"book {book_id} chapter 0: word_count={wc} but no content/content_html"
            )

    # --- Sync-data checks ---------------------------------------------------
    # Multi-file audiobooks have sync attached to the first book only. Probe
    # there and verify the payload isn't zero-filled (schema-mismatch symptom).
    if w.get("has_audio") and w.get("audio_files"):
        first_audio = w["audio_files"][0]
        sd = fetch(f"{base}/api/works/{w['id']}/sync/{first_audio['id']}/0")

        if isinstance(sd, list) and len(sd) > 0:
            fw = sd[0]
            if (
                fw.get("s", 0) == 0
                and fw.get("e", 0) == 0
                and fw.get("w", "") == ""
            ):
                failures.append(
                    f"work {w['id']}: sync_data first-word is zero-filled "
                    f"(schema mismatch on import)"
                )

    if failures:
        print(f"\n✗ {label}")
        for f in failures:
            print(f"    {f}")
    else:
        print(f"✓ {label}")

    return failures


def main() -> int:
    base = sys.argv[1] if len(sys.argv) > 1 else "http://localhost:7654"
    works = fetch(f"{base}/api/works")
    if not isinstance(works, list):
        print(f"✗ /api/works returned non-array: {works}")
        return 1
    print(f"\n→ {len(works)} works to check\n")

    all_failures = 0
    for w in works:
        all_failures += len(check_work(base, w))

    print(f"\n=== {all_failures} failure(s) across {len(works)} works ===")
    return 1 if all_failures else 0


if __name__ == "__main__":
    sys.exit(main())
