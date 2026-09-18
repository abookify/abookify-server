import sys, io, zipfile, hashlib, json, re
sys.path.insert(0, sys.argv[1]); from remote_abook_audit import resolve, central_dir, fetch_entry, BASE
for nm in sys.argv[2:]:
    url,size=resolve(BASE+nm); ents=central_dir(url,size); byname={e[0]:e for e in ents}
    rep={'file':nm}
    if 'cover.jpg' not in byname: rep['cover']='none in .abook'; print(json.dumps(rep)); continue
    cov=fetch_entry(url,byname['cover.jpg']); ch=hashlib.sha256(cov).hexdigest(); rep['cover_bytes']=len(cov)
    match=None; ids=[]
    for e in ents:
        if not e[0].startswith('originals/') or not e[0].endswith('.epub'): continue
        epub=fetch_entry(url,e); z=zipfile.ZipFile(io.BytesIO(epub))
        for n in z.namelist():
            if n.lower().endswith(('.jpg','.jpeg','.png')):
                d=z.read(n)
                if hashlib.sha256(d).hexdigest()==ch: match=e[0]+'!'+n
                elif len(d)==len(cov): match=match or (e[0]+'!'+n+' (same size)')
        for n in z.namelist():
            if n.lower().endswith('.opf'):
                opf=z.read(n).decode('utf-8','replace'); ids+=re.findall(r'gutenberg\.org/(?:ebooks|files)/(\d+)', opf)[:1]; ids+=re.findall(r'<dc:identifier[^>]*>([^<]{0,80})</dc:identifier>',opf)[:2]
    rep['cover_matches_epub_image']=match; rep['epub_identifiers']=ids[:3]
    print(json.dumps(rep,ensure_ascii=False)); sys.stdout.flush()
