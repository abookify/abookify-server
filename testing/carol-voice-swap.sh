#!/usr/bin/env bash
# Regenerate the featured clean-Carol .abook in a different Kokoro voice —
# one command, not a project. Generates a NEW edition (keyed by voice, the
# existing one is untouched), waits, links, exports one-narration-one-text,
# and VERIFIES (span vs decoded, word-count match, monotonicity) before
# declaring anything done. The final import-and-watch check stays manual —
# that standard is the reason the first Carol shipped correct.
#
#   testing/carol-voice-swap.sh bm_lewis
set -euo pipefail
V="${1:?usage: carol-voice-swap.sh <kokoro-voice>}"
cd "$(dirname "$0")/.."
DB=./data/abookify.db
TOK=$(python3 -c "
import sqlite3
con=sqlite3.connect('file:${DB}?mode=ro',uri=True)
print(con.execute('select token from auth_sessions order by created_at desc limit 1').fetchone()[0])")

echo "== queueing generation (work 85, epub 111504, voice $V)"
curl -sf -H "Authorization: Bearer $TOK" -H "Content-Type: application/json" \
  -d "{\"voice\":\"$V\",\"text_book_id\":111504}" \
  http://localhost:7654/api/works/85/generate-audio; echo
JOB="tts-85-111504-$(echo "$V" | tr -c 'a-z0-9' '-' | sed 's/-$//')"
echo "== waiting for $JOB (a full generation takes ~2.5h)"
while :; do
  ST=$(curl -sf -H "Authorization: Bearer $TOK" http://localhost:7654/api/jobs | python3 -c "
import json,sys
js=[j for j in json.load(sys.stdin) if j.get('id')=='$JOB']
print(js[0]['status'] if js else 'missing')")
  echo "  $ST $(date +%H:%M)"
  [ "$ST" = "completed" ] && break
  [ "$ST" = "failed" ] && { echo "GENERATION FAILED"; exit 1; }
  sleep 300
done

echo "== materializing chapter links + collecting book ids"
IDS=$(python3 - "$V" <<'PY'
import sqlite3,sys
v=sys.argv[1]
con=sqlite3.connect('./data/abookify.db'); con.execute('PRAGMA busy_timeout=15000')
slug=''.join(c if c.isalnum() else '-' for c in v.lower())
rows=con.execute("select id,filename from books where work_id=85 and path like ?",
    (f'/generated/tts-book-111504-{slug}/%',)).fetchall()
assert len(rows)==6, f"expected 6 chapter books, found {len(rows)}"
ids=[]
for bid,fn in sorted(rows,key=lambda r:r[1]):
    idx=int(fn.split('-')[1].split('.')[0])
    con.execute("insert or replace into chapter_links (work_id,audio_book_id,audio_index,text_book_id,text_index,confidence) values (85,?,?,111504,?,1.0)",(bid,idx,idx))
    ids.append(str(bid))
con.commit()
print(','.join(ids))
PY
)
echo "   books: $IDS"

OUT="/home/pj/abookify-showcase/A Christmas Carol - Charles Dickens (AI-narrated, $V).abook"
echo "== exporting one-narration-one-text"
curl -sf -H "Authorization: Bearer $TOK" -o "$OUT" \
  "http://localhost:7654/api/works/85/export.abook?audio=1&books=111504,$IDS"
ls -la "$OUT"

echo "== verifying inside the container"
python3 - "$OUT" <<'PY'
import sqlite3,json,os,sys,zipfile,tempfile,subprocess
out=sys.argv[1]
d=tempfile.mkdtemp()
zipfile.ZipFile(out).extractall(d)
con=sqlite3.connect(os.path.join(d,'book.db'))
wc={i:w for i,w in con.execute("select index_num,word_count from chapters where book_id=111504")}
ok=True
for bid,fn,ap in con.execute("select id,filename,asset_path from books where media_type='audio' order by filename"):
    (t,)=con.execute("select timestamps from sync where audio_book_id=?",(bid,)).fetchone()
    ts=json.loads(t)
    mono=all(ts[i+1]['s']>=ts[i]['s']-0.001 for i in range(len(ts)-1))
    r=subprocess.run(['docker','run','--rm','-v',f'{os.path.join(d,ap)}:/a.mp3','--entrypoint','sh','linuxserver/ffmpeg','-c',
        'ffmpeg -v error -i /a.mp3 /tmp/a.wav 2>/dev/null && ffprobe -v error -show_entries format=duration -of csv=p=0 /tmp/a.wav'],
        capture_output=True,text=True)
    dd=float(r.stdout.strip() or 0)
    idx=int(fn.split('-')[1].split('.')[0])
    good=dd>0 and ts[-1]['e']>=0.95*dd and mono and len(ts)==wc.get(idx)
    print(f"  {fn}: words={len(ts)}/{wc.get(idx)} span={ts[-1]['e']:.0f}/{dd:.0f}s mono={mono} [{'OK' if good else 'FAIL'}]")
    ok=ok and good
print('VERDICT:','VERIFIED' if ok else 'FAILED — DO NOT SHIP')
sys.exit(0 if ok else 1)
PY
echo "== done. FINAL GATE (manual, non-negotiable): import into a fresh server and WATCH the words track."
