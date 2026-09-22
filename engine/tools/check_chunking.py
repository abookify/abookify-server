#!/usr/bin/env python3
"""Regression guard for the engine's kokoro-fastapi chunker port (board 17).

The GPU render is only "the same audio" as the Docker TTS while both cut the
text at the same places. This replays recorded inputs through
kfa_chunking.smart_split and compares against splits that were verified
against the running kokoro-fastapi container's own chunk log. Run with the
bundle's python (needs the espeak wheel + inflect):

  engine/dist/engine/python/bin/python3 engine/tools/check_chunking.py

Exit 0 = identical; 1 = a split moved (print shows where). If upstream
kokoro-fastapi changes its chunking, re-verify against the container and
regenerate the fixture — do not just update the expected values.
"""
import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.dirname(HERE))

import kfa_chunking as k  # noqa: E402

FIXTURES = [os.path.join(HERE, "fixtures", "chunking-carol-stave-one.json")]


def main() -> int:
    bad = 0
    total = 0
    for path in FIXTURES:
        cases = json.load(open(path))["cases"]
        for i, case in enumerate(cases):
            got = [t for t, p in k.smart_split(case["input"], lang_code="b") if p is None]
            total += 1
            if got != case["chunks"]:
                bad += 1
                print(f"MISMATCH {os.path.basename(path)} case {i}: {case['input'][:60]!r}")
                for a, b in zip(case["chunks"], got):
                    if a != b:
                        print(f"  expected: {a[:80]!r}\n  got:      {b[:80]!r}")
                        break
                if len(got) != len(case["chunks"]):
                    print(f"  expected {len(case['chunks'])} chunks, got {len(got)}")
    print(f"chunking: {total - bad}/{total} cases identical")
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
