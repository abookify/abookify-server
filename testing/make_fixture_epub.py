#!/usr/bin/env python3
"""Generate a tiny but VALID EPUB so the endpoint smoke (and CI) has a real
work to exercise per-work routes — coverage, diff, cast, qa-suggestions,
text-sync, chapters — without committing a binary blob to the repo.

Usage: make_fixture_epub.py <output-dir>
Writes <output-dir>/smoke-book/smoke.epub (one work, two text chapters).
"""
from __future__ import annotations
import os
import sys
import zipfile

CONTAINER = """<?xml version="1.0" encoding="UTF-8"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles>
    <rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/>
  </rootfiles>
</container>
"""

OPF = """<?xml version="1.0" encoding="UTF-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="2.0" unique-identifier="bookid">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>Smoke Test Book</dc:title>
    <dc:creator>Endpoint Smoke</dc:creator>
    <dc:identifier id="bookid">urn:uuid:abookify-smoke-fixture-0001</dc:identifier>
    <dc:language>en</dc:language>
  </metadata>
  <manifest>
    <item id="ncx" href="toc.ncx" media-type="application/x-dtbncx+xml"/>
    <item id="ch1" href="ch1.xhtml" media-type="application/xhtml+xml"/>
    <item id="ch2" href="ch2.xhtml" media-type="application/xhtml+xml"/>
  </manifest>
  <spine toc="ncx">
    <itemref idref="ch1"/>
    <itemref idref="ch2"/>
  </spine>
</package>
"""

NCX = """<?xml version="1.0" encoding="UTF-8"?>
<ncx xmlns="http://www.daisy.org/z3986/2005/ncx/" version="2005-1">
  <head><meta name="dtb:uid" content="urn:uuid:abookify-smoke-fixture-0001"/></head>
  <docTitle><text>Smoke Test Book</text></docTitle>
  <navMap>
    <navPoint id="n1" playOrder="1"><navLabel><text>Chapter One</text></navLabel><content src="ch1.xhtml"/></navPoint>
    <navPoint id="n2" playOrder="2"><navLabel><text>Chapter Two</text></navLabel><content src="ch2.xhtml"/></navPoint>
  </navMap>
</ncx>
"""


def chapter(title: str, n: int) -> str:
    # Enough words that chapter extraction + cast text-gathering are non-empty.
    body = " ".join(f"This is sentence {i} of {title.lower()}." for i in range(1, 40))
    return f"""<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE html>
<html xmlns="http://www.w3.org/1999/xhtml"><head><title>{title}</title></head>
<body><h1>{title}</h1><p>{body}</p>
<p>Winston Smith walked toward the centre, recognised the grey light, and travelled on.</p></body></html>
"""


def main() -> int:
    if len(sys.argv) < 2:
        print("usage: make_fixture_epub.py <output-dir>", file=sys.stderr)
        return 2
    out_dir = os.path.join(sys.argv[1], "smoke-book")
    os.makedirs(out_dir, exist_ok=True)
    path = os.path.join(out_dir, "smoke.epub")
    with zipfile.ZipFile(path, "w", zipfile.ZIP_DEFLATED) as z:
        # mimetype MUST be first and stored uncompressed per the EPUB spec.
        z.writestr(zipfile.ZipInfo("mimetype"), "application/epub+zip",
                   compress_type=zipfile.ZIP_STORED)
        z.writestr("META-INF/container.xml", CONTAINER)
        z.writestr("OEBPS/content.opf", OPF)
        z.writestr("OEBPS/toc.ncx", NCX)
        z.writestr("OEBPS/ch1.xhtml", chapter("Chapter One", 1))
        z.writestr("OEBPS/ch2.xhtml", chapter("Chapter Two", 2))
    print(path)
    return 0


if __name__ == "__main__":
    sys.exit(main())
