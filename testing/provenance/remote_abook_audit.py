#!/usr/bin/env python3
"""Inspect .abook zips on GitHub releases WITHOUT downloading their audio: read the
zip central directory via HTTP Range, then fetch only manifest.json, ATTRIBUTION.txt,
cover.jpg and book.db. Reports the equal-split tell on audio entry sizes."""
import sys, json, struct, zlib, hashlib, sqlite3, tempfile, os, urllib.request, subprocess
BASE='https://github.com/abookify/abookify-server/releases/download/showcase-v1/'
def resolve(url):
    req=urllib.request.Request(url, method='HEAD'); 
    with urllib.request.urlopen(req) as r: return r.geturl(), int(r.headers.get('Content-Length','0'))
def rng(url,a,b):
    req=urllib.request.Request(url, headers={'Range':f'bytes={a}-{b}'})
    with urllib.request.urlopen(req) as r: return r.read()
def central_dir(url,size):
    tail=rng(url,max(0,size-70000),size-1); i=tail.rfind(b'PK\x05\x06')
    if i<0: raise RuntimeError('no EOCD')
    n,cdsize,cdoff=struct.unpack('<HII',tail[i+10:i+20])
    if cdoff==0xFFFFFFFF or n==0xFFFF:  # zip64
        j=tail.rfind(b'PK\x06\x06'); cdsize,cdoff=struct.unpack('<QQ',tail[j+40:j+56])
    cd=rng(url,cdoff,cdoff+cdsize-1); entries=[]; p=0
    while p+46<=len(cd) and cd[p:p+4]==b'PK\x01\x02':
        comp,=struct.unpack('<H',cd[p+10:p+12]); csz,usz=struct.unpack('<II',cd[p+20:p+28]); nl,el,cl=struct.unpack('<HHH',cd[p+28:p+34]); off,=struct.unpack('<I',cd[p+42:p+46])
        name=cd[p+46:p+46+nl].decode('utf-8','replace'); extra=cd[p+46+nl:p+46+nl+el]
        if 0xFFFFFFFF in (csz,usz,off):  # zip64 extra
            q=0
            while q+4<=len(extra):
                hid,hl=struct.unpack('<HH',extra[q:q+4]); d=extra[q+4:q+4+hl]
                if hid==1:
                    vals=list(struct.unpack('<'+'Q'*(len(d)//8),d[:len(d)//8*8])); k=0
                    if usz==0xFFFFFFFF: usz=vals[k]; k+=1
                    if csz==0xFFFFFFFF: csz=vals[k]; k+=1
                    if off==0xFFFFFFFF: off=vals[k]; k+=1
                q+=4+hl
        entries.append((name,comp,csz,usz,off)); p+=46+nl+el+cl
    return entries
def fetch_entry(url,e):
    name,comp,csz,usz,off=e; lh=rng(url,off,off+29); nl,el=struct.unpack('<HH',lh[26:30]); start=off+30+nl+el
    data=rng(url,start,start+csz-1)
    return zlib.decompress(data,-15) if comp==8 else data
if __name__ == '__main__':
    site_covers={}
    cdir='/home/pj/projects/jarvis/abookify/marketing/site/showcase/covers'
    for f in os.listdir(cdir): site_covers[hashlib.sha256(open(os.path.join(cdir,f),'rb').read()).hexdigest()]=f
    names=sys.argv[1:]
    for nm in names:
        url,size=resolve(BASE+nm); ents=central_dir(url,size); byname={e[0]:e for e in ents}
        audio=[e for e in ents if e[0].startswith('audio/')]; sizes=[e[3] for e in audio]
        equal=len(sizes)>1 and max(sizes)-min(sizes)<=1
        rep={'file':nm,'bytes':size,'audio_files':len(audio),'audio_sizes_MB':sorted(round(s/1e6,1) for s in sizes)[:8],'equal_split':equal,
             'originals':[e[0] for e in ents if e[0].startswith('originals/')]}
        if 'manifest.json' in byname:
            m=json.loads(fetch_entry(url,byname['manifest.json'])); rep['title']=m.get('title'); rep['provenance']=m.get('provenance'); rep['source_kind']=m.get('source_kind'); rep['content_version']=m.get('content_version')
        if 'ATTRIBUTION.txt' in byname:
            a=fetch_entry(url,byname['ATTRIBUTION.txt']).decode('utf-8','replace'); rep['attribution']=[l.strip() for l in a.splitlines() if any(k in l for k in ('Source:','Reader:','Type:','Status:','voice','Recording:','Archive'))][:8]
        else: rep['attribution']='MISSING'
        if 'cover.jpg' in byname:
            h=hashlib.sha256(fetch_entry(url,byname['cover.jpg'])).hexdigest(); rep['cover_on_site']=site_covers.get(h,'(not a site cover)')
        if 'book.db' in byname:
            d=fetch_entry(url,byname['book.db']); t=tempfile.NamedTemporaryFile(delete=False,suffix='.db'); t.write(d); t.close()
            c=sqlite3.connect(t.name); rows=c.execute("select origin, album, edition, filename, round(duration) from books where asset_path is not null and asset_path!='' order by id").fetchall()
            rep['audio_books']=[(o,al,ed,fn[:40],du) for o,al,ed,fn,du in rows[:4]]; rep['audio_origins']=sorted({r[0] for r in rows})
            rep['text_books']=c.execute("select format, origin, filename from books where asset_path is null or asset_path=''").fetchall(); c.close(); os.unlink(t.name)
        print(json.dumps(rep,ensure_ascii=False)); sys.stdout.flush()
