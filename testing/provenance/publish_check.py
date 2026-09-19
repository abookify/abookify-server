#!/usr/bin/env python3
"""publish-check — the gate at the two places publishing happens.

  publish_check.py abook FILE.abook [...]     # local files, or showcase-v1 asset names with --remote
  publish_check.py site  SITE_DIR             # marketing/site: every cover/sample/shot must be in PROVENANCE.json, cleared, hash-matched

Exit 0 only when every artifact is GREEN. Any red → exit 1 with the reason. It checks
CLEARED, not declared: a manifest with attribution text but no `publishing` block whose
every source is cleared=true (with cleared_by) is red — that is exactly the Owl Creek case.
"""
import sys, os, json, io, zipfile, hashlib, re, sqlite3, tempfile

ALLOWED_KINDS = {'gutenberg','librivox','pg-open-audiobook','kokoro','cc0','cc-by','own','public-domain'}

def sha(b): return hashlib.sha256(b).hexdigest()

def check_abook_zip(name, z, audio_sizes=None):
    reds, notes = [], []
    names = z.namelist()
    if 'manifest.json' not in names: return ['no manifest.json'], notes
    m = json.loads(z.read('manifest.json'))
    pub = m.get('publishing')
    if not pub or not pub.get('public'):
        reds.append('no `publishing` block — this file was not produced by a PUBLIC export; not cleared for redistribution (attribution text alone does not count)')
    else:
        for s in pub.get('sources', []):
            if not s.get('cleared') or not s.get('cleared_by'):
                reds.append(f"source book {s.get('book_id')} ({s.get('media')} {s.get('format')}) declared kind={s.get('kind')!r} but NOT cleared")
            elif (s.get('kind') or '').lower() not in ALLOWED_KINDS and not s.get('note'):
                reds.append(f"source book {s.get('book_id')} kind={s.get('kind')!r} is outside the recognised public-domain kinds and carries no note explaining the clearance")
        cov = pub.get('cover') or {}
        if 'cover.jpg' in names and not cov.get('cleared'):
            reds.append('cover.jpg is bundled but the publishing block does not record a cleared cover')
        if 'cover.jpg' not in names and cov.get('bundled'):
            reds.append('publishing block says the cover was bundled but cover.jpg is absent')
    # physical tells (independent of declarations)
    audio = [n for n in names if n.startswith('audio/')]
    sizes = audio_sizes if audio_sizes is not None else [z.getinfo(n).file_size for n in audio]
    if len(sizes) > 1 and max(sizes) - min(sizes) <= 1:
        reds.append(f'EQUAL-SPLIT audio: {len(sizes)} files of identical size — the commercial-rip signature')
    if 'ATTRIBUTION.txt' not in names:
        reds.append('no ATTRIBUTION.txt')
    # cover must be the bundled EPUB's own image unless explicitly cleared
    if 'cover.jpg' in names:
        ch = sha(z.read('cover.jpg')); matched = False
        for n in names:
            if n.startswith('originals/') and n.endswith('.epub'):
                try:
                    ez = zipfile.ZipFile(io.BytesIO(z.read(n)))
                    for en in ez.namelist():
                        if en.lower().endswith(('.jpg','.jpeg','.png')) and sha(ez.read(en)) == ch: matched = True
                except Exception: pass
        cov = (pub or {}).get('cover') or {}
        if not matched and not (cov.get('cleared') and cov.get('cleared_by')):
            reds.append('cover.jpg matches no image inside the bundled EPUB and has no explicit clearance (this is how two OpenLibrary publisher covers shipped)')
        elif matched: notes.append('cover = bundled EPUB\'s own image')
    # text provenance tell: gutenberg id in the bundled epub's OPF
    for n in names:
        if n.startswith('originals/') and n.endswith('.epub'):
            try:
                ez = zipfile.ZipFile(io.BytesIO(z.read(n)))
                opf = ''.join(ez.read(x).decode('utf-8','replace') for x in ez.namelist() if x.lower().endswith('.opf'))
                gid = re.findall(r'gutenberg\.org/(?:ebooks|files)/(\d+)', opf)
                notes.append(f'{n}: gutenberg #{gid[0]}' if gid else f'{n}: no gutenberg identifier in OPF (verify the text source)')
            except Exception as e: notes.append(f'{n}: unreadable ({e})')
    return reds, notes

def check_abook_files(paths, remote=False):
    ok = True
    for p in paths:
        if remote:
            sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
            from remote_abook_audit import resolve, central_dir, fetch_entry, BASE
            url, size = resolve(BASE + p); ents = central_dir(url, size)
            buf = io.BytesIO(); zw = zipfile.ZipFile(buf, 'w')
            for e in ents:
                if e[0] in ('manifest.json','ATTRIBUTION.txt','cover.jpg') or e[0].startswith('originals/'):
                    zw.writestr(e[0], fetch_entry(url, e))
            for e in ents:
                if e[0].startswith('audio/'): zw.writestr(zipfile.ZipInfo(e[0]), b'')  # names only
            zw.close(); z = zipfile.ZipFile(io.BytesIO(buf.getvalue()))
            # keep real audio sizes for the equal-split tell
            sizes = [e[3] for e in ents if e[0].startswith('audio/')]  # real sizes from the central directory
            reds, notes = check_abook_zip(p, z, audio_sizes=sizes)
        else:
            z = zipfile.ZipFile(p); reds, notes = check_abook_zip(p, z)
        print(('RED   ' if reds else 'GREEN ') + os.path.basename(p))
        for r in reds: print('   ✗ ' + r)
        for n in notes: print('   · ' + n)
        ok = ok and not reds
    return ok

def check_site(site_dir):
    ledger_p = os.path.join(site_dir, 'PROVENANCE.json')
    if not os.path.exists(ledger_p):
        print('RED   site: no PROVENANCE.json ledger'); return False
    ledger = json.load(open(ledger_p)); entries = {e['path']: e for e in ledger.get('assets', [])}
    ok = True
    for sub in ('showcase/covers', 'showcase/samples', 'shots'):
        d = os.path.join(site_dir, sub)
        if not os.path.isdir(d): continue
        for f in sorted(os.listdir(d)):
            rel = f'{sub}/{f}'
            if f.endswith('.json'): continue
            e = entries.get(rel)
            h = sha(open(os.path.join(d, f), 'rb').read())
            if not e: print(f'RED   {rel}: not in PROVENANCE.json'); ok = False; continue
            if e.get('sha256') != h: print(f'RED   {rel}: hash differs from the ledger (file changed since it was cleared)'); ok = False; continue
            if not (e.get('cleared') and e.get('cleared_by')): print(f'RED   {rel}: in the ledger but not cleared'); ok = False; continue
            print(f'GREEN {rel}  ({e.get("source")})')
    for rel in entries:
        if not os.path.exists(os.path.join(site_dir, rel)): print(f'note  {rel}: in the ledger but not on disk (stale entry)')
    return ok

if __name__ == '__main__':
    if len(sys.argv) < 3: print(__doc__); sys.exit(2)
    mode = sys.argv[1]; args = [a for a in sys.argv[2:] if a != '--remote']; remote = '--remote' in sys.argv
    good = check_abook_files(args, remote) if mode == 'abook' else check_site(args[0]) if mode == 'site' else None
    if good is None: print(__doc__); sys.exit(2)
    print('\nPUBLISH-CHECK: ' + ('PASS — every artifact is cleared' if good else 'FAIL — do not publish'))
    sys.exit(0 if good else 1)
