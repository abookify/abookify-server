#!/usr/bin/env python3
# INDEPENDENT .abook coherence validator (server-web). Does NOT call BuildWorkCanon.
# The invariant, checked from the raw book.db: a single NARRATION — identified by its
# (origin, voice/album) — must carry ONE edition label, and one edition label must not
# span two narrations. A split (same narration, multiple labels) is what shipped the
# two-editions-of-three bug into a derived copy. Grouping by (origin,album) rather than
# canon's audio-directory is deliberate: it is a different signal (and .abook flattens
# every file into audio/, where directory grouping is meaningless anyway).
import sys, zipfile, sqlite3, tempfile, os
def validate(path):
    problems=[]
    try:
        with zipfile.ZipFile(path) as z:
            if 'book.db' not in z.namelist():
                return ['no book.db in archive'], []
            with tempfile.NamedTemporaryFile(suffix='.db', delete=False) as tf:
                tf.write(z.read('book.db')); dbp=tf.name
    except Exception as e:
        return [f'cannot open archive: {e}'], []
    try:
        c=sqlite3.connect(dbp)
        rows=list(c.execute("SELECT id, edition, album, origin FROM books WHERE media_type='audio'"))
    finally:
        os.unlink(dbp)
    # group by narration = (origin, album)
    by_narr={}
    for bid,ed,al,orig in rows:
        by_narr.setdefault((orig,al),{'labels':set(),'n':0})
        by_narr[(orig,al)]['labels'].add(ed or '')
        by_narr[(orig,al)]['n']+=1
    for (orig,al),info in by_narr.items():
        nonempty=[l for l in info['labels'] if l]
        if len(info['labels'])>1:
            problems.append(f"edition-label split: narration ({orig}, voice={al!r}) x{info['n']} carries {len(info['labels'])} labels {sorted(info['labels'])}")
    # a single non-empty label spanning two narrations
    label_narr={}
    for (orig,al),info in by_narr.items():
        for l in info['labels']:
            if l: label_narr.setdefault(l,set()).add((orig,al))
    for l,narrs in label_narr.items():
        if len(narrs)>1:
            problems.append(f"label {l!r} spans {len(narrs)} narrations {sorted(narrs)}")
    return problems, [f"{k}: {v['n']} files, labels={sorted(v['labels'])}" for k,v in by_narr.items()]

if __name__=='__main__':
    files=sys.argv[1:]
    incoherent=0
    for f in sorted(files):
        probs,summary=validate(f)
        name=os.path.basename(f)
        if probs:
            incoherent+=1
            print(f"  INCOHERENT  {name}")
            for p in probs: print(f"                -> {p}")
        else:
            print(f"  coherent    {name}  ({'; '.join(summary) or 'no audio'})")
    print(f"\n=== {len(files)} artifacts swept: {len(files)-incoherent} coherent, {incoherent} INCOHERENT ===")
    sys.exit(1 if incoherent else 0)
