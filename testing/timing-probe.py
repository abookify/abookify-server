#!/usr/bin/env python3
"""Library-wide audio-timing soundness probe.

For each narration edition: pick 3 deterministic 20s windows (20/50/80% of the
edition), extract the actual audio, re-transcribe on GPU whisper, and compare
against the words+times the stored word map claims for that window.

PASS window = >=60% of whisper's distinctive words found in the map's window
with the same token, AND median |time delta| over matches <= 1.5s.

This measures TIMING CORRECTNESS (does the map put the right words at the
right seconds), which neither flagged% (text divergence) nor
span/count/monotonicity (timeline plausibility) measures.
"""
import sqlite3,json,os,subprocess,time,statistics,re,sys,urllib.request,io

DB=os.environ.get('PROBE_DB','/home/pj/projects/jarvis/abookify/engineering/server/data/abookify.db')
OUT=os.environ.get('PROBE_OUT') or os.path.expanduser('~/tmp/claude-1000/-home-pj-projects-jarvis-abookify-engineering-server/77ca4a16-7ccd-4b76-bfa8-6d948ce25840/scratchpad/timing-probe-results.tsv')
WIN=20.0
FRACS=(0.2,0.5,0.8)

def norm(w):
    return re.sub(r'[^a-z0-9]','',w.lower())

def extract(container_path, off, out_host):
    # Host-local paths (fixture servers) are cut with a throwaway ffmpeg
    # container; the main server's /library and /generated paths are cut
    # inside the server container.
    if os.path.exists(container_path):
        r=subprocess.run(['docker','run','--rm','-v',f'{container_path}:/in.mp3','-v',
            f'{os.path.dirname(out_host)}:/out','--entrypoint','ffmpeg','linuxserver/ffmpeg',
            '-y','-v','error','-ss',f'{off:.2f}','-t',f'{WIN:.0f}','-i','/in.mp3',
            '-c:a','libmp3lame','-q:a','5',f'/out/{os.path.basename(out_host)}'],capture_output=True,text=True)
        return r.returncode==0
    r=subprocess.run(['docker','exec','server-server-1','ffmpeg','-y','-v','error',
        '-ss',f'{off:.2f}','-t',f'{WIN:.0f}','-i',container_path,
        '-c:a','libmp3lame','-q:a','5','/tmp/probe-clip.mp3'],capture_output=True,text=True)
    if r.returncode!=0: return False
    r=subprocess.run(['docker','cp','server-server-1:/tmp/probe-clip.mp3',out_host],capture_output=True)
    return r.returncode==0

def transcribe(path):
    import mimetypes
    boundary='----probe'
    data=open(path,'rb').read()
    body=(f'--{boundary}\r\nContent-Disposition: form-data; name="file"; filename="c.mp3"\r\n'
          f'Content-Type: audio/mpeg\r\n\r\n').encode()+data+f'\r\n--{boundary}--\r\n'.encode()
    req=urllib.request.Request('http://localhost:5200/transcribe',data=body,
        headers={'Content-Type':f'multipart/form-data; boundary={boundary}'})
    d=json.load(urllib.request.urlopen(req,timeout=300))
    return [(w['word'],w['start']) for s in d.get('segments',[]) for w in s.get('words',[])]

def compare(map_words, heard, t0):
    """map_words: [(token, abs_sec)], heard: [(word, rel_sec)]"""
    deltas=[]; matched=0; considered=0
    used=set()
    for hw,hs in heard:
        tok=norm(hw)
        if len(tok)<4: continue
        considered+=1
        habs=t0+hs
        best=None;bestd=None
        for i,(mt,ms) in enumerate(map_words):
            if i in used or mt!=tok: continue
            d=abs(ms-habs)
            if d<6 and (bestd is None or d<bestd): best,bestd=i,d
        if best is not None:
            used.add(best); matched+=1; deltas.append(bestd)
    rate=matched/considered if considered else 0
    med=statistics.median(deltas) if deltas else None
    return rate,med,considered

con=sqlite3.connect(f'file:{DB}?mode=ro',uri=True)
works={r[0]:r[1] for r in con.execute('select id,title from works')}
res=open(OUT,'w')
res.write("work\ttitle\tedition\twindows\tpassed\tmatch_rates\tmedian_deltas\n")
t_start=time.time(); nwin=0
for wid,title in sorted(works.items()):
    books=con.execute("select id,path,filename,duration from books where work_id=? and media_type='audio' order by filename",(wid,)).fetchall()
    if not books: continue
    editions={}
    for bid,path,fn,dur in books:
        editions.setdefault(os.path.dirname(path),[]).append((bid,path,fn,dur or 0))
    for edir,files in editions.items():
        rows=con.execute("select audio_book_id,timestamps from sync_data where work_id=?",(wid,)).fetchall()
        byid={}
        for abid,ts in rows:
            if abid in [f[0] for f in files]:
                byid.setdefault(abid,[]).append(ts)
        if not byid: continue
        blob_mode = len(byid)==1 and len(files)>1 and len(list(byid.values())[0])==1
        total=sum(f[3] for f in files)
        if total<90: continue
        passed=0;rates=[];meds=[];wins=0
        for frac in FRACS:
            if blob_mode:
                T0=total*frac
                # map words from the single blob (book-continuous)
                ts=json.loads(list(byid.values())[0][0])
                mw=[(norm(w['w']),w['s']) for w in ts if T0-3<=w['s']<=T0+WIN+3 and len(norm(w['w']))>=4]
                # locate file
                acc=0;target=None;off=None
                for bid,path,fn,dur in files:
                    if T0 < acc+dur: target,off=path,T0-acc; break
                    acc+=dur
            else:
                # per-file rows (TTS) — pick file by fraction of edition
                acc=0;target=None;off=None;row=None
                T0=total*frac
                for bid,path,fn,dur in files:
                    if T0 < acc+dur:
                        target,off=path,T0-acc
                        row=byid.get(bid)
                        break
                    acc+=dur
                if not row: continue
                ts=json.loads(row[0])
                mw=[(norm(w['w']),w['s']) for w in ts if off-3<=w['s']<=off+WIN+3 and len(norm(w['w']))>=4]
            if target is None or off is None or off+WIN>= (dur if not blob_mode else 10**9):
                pass
            clip=os.path.expanduser('~/tmp/claude-1000/-home-pj-projects-jarvis-abookify-engineering-server/77ca4a16-7ccd-4b76-bfa8-6d948ce25840/scratchpad/probe-clip.mp3')
            if not extract(target,off,clip): continue
            try: heard=transcribe(clip)
            except Exception as e: continue
            rate,med,considered=compare(mw,heard,off if not blob_mode else T0)
            if considered<10: continue
            wins+=1;nwin+=1
            rates.append(f'{rate:.2f}');meds.append(f'{med:.2f}' if med is not None else '-')
            if rate>=0.6 and med is not None and med<=1.5: passed+=1
        if wins:
            res.write(f"{wid}\t{title[:38]}\t{os.path.basename(edir)}\t{wins}\t{passed}\t{','.join(rates)}\t{','.join(meds)}\n")
            res.flush()
res.write(f"# {nwin} windows in {time.time()-t_start:.0f}s\n")
res.close()
print("done", nwin, "windows in", f"{time.time()-t_start:.0f}s")
