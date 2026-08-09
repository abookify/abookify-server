#!/usr/bin/env node
// Calibration for karaoke_advances (A1-A5): drive the REAL shipped-broken
// state (reader on the title chapter while Stave One plays) and the good
// state, and demand FAIL on the first, PASS on the second. A check that has
// only ever seen passing input has not been checked.
const PW = '/home/pj/projects/web/youarehereart/laravel-web/frontend/node_modules/playwright-core';
const { chromium } = require(PW);
const BASE = process.argv[2] || 'http://localhost:8199';
const MODE = process.argv[3] || 'broken'; // broken | good | empty | shift | clockfreeze
const clockSecs = t => { const p=(t||'').trim().split(':').map(Number); return p.some(isNaN)||!p.length?null:p.reduce((a,b)=>a*60+b,0); };
(async () => {
  const browser = await chromium.launch({ executablePath: '/usr/bin/chromium', args: ['--no-sandbox','--autoplay-policy=no-user-gesture-required'] });
  const page = await (await browser.newContext({ viewport:{width:1400,height:900} })).newPage();
  await page.goto(BASE, { waitUntil: 'networkidle' });
  await page.locator('.work-card, [class*=card]').first().click();
  await page.waitForTimeout(2000);
  await page.locator('text=/STAVE\\s+ONE/i').first().click(); // start stave-one AUDIO (reader opens on ch0)
  await page.waitForTimeout(1000);
  const play = page.locator('button:has-text("▶"), [aria-label*="play" i]').first();
  if (await play.count()) await play.click().catch(()=>{});
  await page.waitForTimeout(2000);
  if (MODE !== 'broken') { // navigate the READER to the narrated chapter
    for (const r of await page.locator('text=/STAVE\\s+ONE/i').all()) {
      const b = await r.boundingBox();
      if (b && b.x > 700) { await r.click(); break; }
    }
    await page.waitForTimeout(3000);
  }
  if (MODE === 'empty') { // switch the reader to the (mangled) transcript source
    const val = await page.evaluate(() => {
      const sel = document.getElementById('np-text-source');
      const opt = sel ? [...sel.options].find(o => /transcript/i.test(o.textContent)) : null;
      return opt ? opt.value : null;
    });
    if (val) await page.selectOption('#np-text-source', val);
    await page.waitForTimeout(4000);
  }
  // NOTE on the wrong-sentence state: a UNIFORMLY shifted word map stays
  // self-consistent (the active index moves with it), so A4 cannot detect it
  // by construction — A1-A5 certify UI-layer sync. Map-vs-AUDIO wrongness is
  // the TIMING PROBE tier's job; its calibration (shift a real sync row 30s,
  // probe must fail) lives in calibrate-probe.sh.
  if (MODE === 'clockfreeze') { // position advances 4x slower than wall time
    await page.evaluate(() => { [...document.querySelectorAll('audio')].forEach(a => a.playbackRate = 0.25); });
    await page.waitForTimeout(1000);
  }
  const state = () => page.evaluate(() => {
    const read=[...document.querySelectorAll('.sync-word.read')].map(e=>+e.dataset.widx).filter(n=>!isNaN(n));
    const active=read.length?Math.min(...read)+1:null;
    const map=(typeof activeSyncData!=='undefined'&&activeSyncData)?activeSyncData:null;
    return { words:document.querySelectorAll('.sync-word').length, active,
             mapSec:(map&&active!=null&&map[active])?map[active].s:null };
  });
  const uiClock = async () => clockSecs(await page.locator('.player-time').first().textContent().catch(()=>null));
  const s1=await state(), c1=await uiClock(), w1=Date.now();
  await page.waitForTimeout(10000);
  const s2=await state(), c2=await uiClock(), w2=Date.now();
  const wall=(w2-w1)/1000;
  const A1=s2.words>=50, A2=s2.active!=null, A3=A2&&s1.active!=null&&s2.active>s1.active;
  const A4=s2.mapSec!=null&&c2!=null&&Math.abs(s2.mapSec-c2)<=2.5;
  const A5=c1!=null&&c2!=null&&Math.abs((c2-c1)-wall)<=2.5;
  const pass=A1&&A2&&A3&&A4&&A5;
  console.log(`[${MODE}] A1=${A1}(${s2.words}w) A2=${A2} A3=${A3}(${s1.active}->${s2.active}) A4=${A4}(map=${s2.mapSec==null?'null':s2.mapSec.toFixed(1)} clock=${c2}) A5=${A5} => ${pass?'PASS':'FAIL'}`);
  await browser.close();
  process.exit(0);
})().catch(e => { console.error('FATAL', e.message); process.exit(2); });
