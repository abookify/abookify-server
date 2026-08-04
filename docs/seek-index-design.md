# Server seek index — design (server-web, for mobile review)

Written 2026-08-04 (server-web). Fixes the VBR-MP3 streamed-seek inaccuracy mobile
proved in `../../mobile/abookify-mobile/docs/seek-accuracy-and-buffering.md`. This
is the **server side of Path A (served seek index)**. **Payload is NOT final until
mobile confirms the consumption path (§4) — do not ship a format mobile hasn't
agreed to consume.**

## 0. Confirmed on the real library (not just theory)
- `ffprobe -show_packets` yields exact **`pts_time,pos`** (time, byte offset) per
  audio frame — a ready-made time→byte table, no custom MP3 parser needed.
- Full scan of a 5.3 h / 76 MB file (`CanneryRow_mp332.mp3`, 531,606 packets):
  **~2.1 s**, once. Cheap enough to cache; too many points to ship raw (downsample).
- **Scope is narrow.** Cannery Row is **CBR** (constant 144-byte frames) → its
  byte↔time is already exact, so ExoPlayer's CBR seek map is fine → **no index
  needed**. Only **VBR MP3 with no Xing/Info/VBRI header** (variable frame sizes,
  e.g. Hitchhiker's) is broken. m4b/m4a (MP4 sample table), opus/ogg (granule) are
  natively seekable → no index. So we scan+index **only headerless VBR MP3**, a
  small slice of the library.

## 1. Endpoint (proposed)
`GET /api/books/{id}/seek-index` — same lifecycle as `GET /api/books/{id}/waveform`
(scan once, cache per file, serve JSON). Single-file today; multi-file work maps to
the book being played, like the per-file waveform.

```jsonc
{
  "book_id": 123,
  "file": "disc1.mp3",
  "codec": "mp3",
  "needs_index": true,          // false => client's native seek is already accurate; ignore the rest
  "reason": "vbr_no_header",     // or "cbr" | "has_xing" | "container_seekable" when needs_index=false
  "duration_sec": 4200.5,
  "total_bytes": 33812480,
  "audio_start_byte": 4096,      // first audio frame (past any ID3v2 tag)
  "granularity_sec": 1.0,        // spacing of the offsets below
  "byte_at_sec": [4096, 8110, 12050, ...]   // byte offset of the frame at/just before N seconds; index = second
}
```
- `byte_at_sec[n]` = byte offset to begin decoding for content-second `n`. For a
  target time T: use `byte_at_sec[floor(T)]` (≤ `granularity_sec` error; the client
  may refine by decoding forward a few frames). At 1 s granularity a 70-min file is
  ~4,200 ints; delta-encoded + HTTP gzip it's on the order of the waveform payload.
  Coarser granularity (2 s) halves it at the cost of worse worst-case error —
  **mobile picks the granularity/precision tradeoff (§4).**
- Two-array `[t[], byte[]]` is the alternative if we ever want non-uniform points;
  uniform `byte_at_sec` is smaller and simpler for v1.

## 2. Building it
- `ffprobe -v error -select_streams a:0 -show_entries packet=pts_time,pos -of csv`
  → downsample to one offset per `granularity_sec` (first frame whose `pts_time ≥ n`).
- Detect `needs_index`: MP3 + no Xing/Info/VBRI in the first frame + **non-constant
  frame spacing** (sample the first ~1000 `pos` deltas; all equal ⇒ CBR ⇒ false).
- Non-MP3 or seekable-container ⇒ `needs_index:false`, no scan.

## 3. Handling expensive scans
- **Cache per file**, keyed by path + size + mtime — mirror the waveform cache
  exactly (same table/sidecar mechanism; I'll match whatever `waveform` uses).
- **Precompute opportunistically**: build the index right after STT / at ingest,
  when we already have the file open — so it's warm before the first seek. Lazy
  build + cache on first request otherwise (block-and-return like the waveform;
  ~2 s worst case, then cached).
- **Most files never scan** (`needs_index:false` short-circuits before ffprobe):
  CBR MP3, files with a Xing TOC, and all non-MP3 containers.
- Bound concurrency (one scan per file at a time; single-flight) so a burst of
  first-touches can't stack N ffprobes.

## 4. THE CONSUMPTION QUESTION — mobile must confirm before I finalize
The index is **time→byte**. But expo-audio's high-level `seekTo(seconds)` does not
accept a byte offset, and the client can't read back the true byte ExoPlayer landed
on — so a JSON time→byte table is only useful **if the client has a way to act on a
byte offset.** Three ways this gets consumed; **mobile owns which:**

- **(A) Client-side byte seek.** Mobile drops to a lower-level player/DataSource
  (below expo-audio's `seekTo`) and, on a seek to T, issues the range request at
  `byte_at_sec[floor(T)]` itself. Keeps ordinary range streaming + HTTP caching
  fully intact (meta's stated goal). **Best outcome — but only if the RN player
  layer exposes byte-level seeking.** Mobile's doc says "the client uses to request
  the correct byte range," which implies this is possible; **confirm it.**
- **(B) Index-backed stream start (fallback if A isn't reachable).** Add
  `GET /api/books/{id}/stream?t=SEC`: the server looks up `byte_at_sec` in the
  **cached** index (O(1)) and streams from that exact frame. The client "seeks" by
  reloading the source at `t`. **Important nuance for meta:** this is *time-addressed
  in form but NOT the "CPU on every seek" thing you rejected* — the expensive part
  (the scan) is one-time and cached; a `?t=` seek is just an array lookup + normal
  byte serving. It does reopen the stream for that one seek (loses that seek's
  in-flight buffer), but steady-state playback stays on ordinary range requests.
- **(C) In-file Xing TOC injection — rejected for this use.** MP3's only native
  index ExoPlayer reads is the Xing TOC: **100 points**, ~tens of seconds per bucket
  on a long book. Player-native but far too coarse for word-level karaoke. Not a fit.

**My recommendation (I own this):** build the §1 index primitive now regardless — it
is the reusable artifact #226 (progressive buffering) also needs, and it matches the
waveform pattern, so meta's preference stands. Then consume via **(A) if mobile's
player can byte-seek; otherwise (B)**, which the *same cached index* powers cheaply.
The index-vs-time-addressed fork meta drew mostly dissolves once the index is cached:
the only real question is whether the RN player can seek by byte (A) or must reload
(B). **I will not finalize `byte_at_sec` granularity or the array encoding until
mobile confirms A vs B and the tradeoff in §1.**

## 5. Open items for the contract (mobile ↔ server-web)
1. **A or B?** (decides whether I also build the `?t=` stream-start.)
2. **Granularity**: 1 s (≤1 s error, larger payload) vs 2 s (smaller, ≤2 s error).
   Karaoke wants ≤1 s; confirm.
3. **Encoding**: plain `byte_at_sec` int array vs delta-encoded — mobile's parse
   preference.
4. **needs_index:false handling**: client must treat it as "seek natively, ignore
   this endpoint" so CBR/seekable files are untouched.
