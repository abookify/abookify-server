# Realtime voice conversation — scope (Gemini Live / OpenAI Realtime)

Status: **SCOPE ONLY — not built.** Authored 2026-07-29 (server-web lane).

## 0. Ground truth first (this determines everything below)

From the task-68 investigation (see `handoff/server-web.md` pm36, verified in code
+ a live test):

- The advertised "Voice Chat" (Settings → Gemini Live / OpenAI Realtime) is
  **nominal** — `voice_provider`/`voice_api_key` are stored but **nothing consumes
  them**.
- A working push-to-talk `POST /api/works/{id}/converse` exists (Whisper→RAG→
  Kokoro), but it is a **discrete request/response round-trip**, architecturally
  unrelated to realtime, and has no client.

**Therefore this is a FROM-SCRATCH build, not an extension.** Realtime speech-to-
speech keeps an open bidirectional audio stream with the model doing speech-in and
speech-out natively — there is no transcribe-then-generate-then-synthesize step to
extend, and no existing realtime scaffolding. What is genuinely reusable:

- the **credential vault** + resolution (pm34) — the key is already stored,
- the **settings entry point** (`voice_provider`/`voice_api_key`),
- **RAG retrieval** (`AskWithCitations` internals) — to ground answers in the book,
- the eligibility/gating mechanism (pm34/pm35) — to light the lane up per key.

Everything on the audio path is new.

## 1. Architectural placement

Realtime voice is **its own lane** — speech-to-speech. It **does NOT belong in the
STT or TTS selectors** and must not be forced into them:

- STT selector = the transcription pipeline (audiobook → text, word timestamps).
- TTS selector = the narration pipeline (ebook → audiobook).
- Realtime voice = a live conversation; it neither transcribes a library file nor
  generates a narration edition. Its home is the existing **"Voice Chat" settings
  group** (`voice_provider`), which already lists `gemini` / `openai-realtime`.

Concretely: add a **`voice` (realtime) capability** to the eligibility model rather
than overloading stt/tts. Today `integratedKinds` covers only llm/stt/tts and
`kindOf` maps only those three; the voice lane adds a 4th kind wired the same way,
so the `voice_provider` dropdown becomes gated + resolved from the vault exactly
like the other three (pm34/pm35), instead of the static ungated select it is now.

## 2. Provider comparison + recommendation

Both are streaming + bidirectional. Both work with a key PJ already has stored
(OpenAI `[llm,stt,tts]`, Google `[llm,voice]`).

| | **OpenAI Realtime** | **Gemini Live** |
|---|---|---|
| Transport (browser) | **WebRTC** (recommended) or WebSocket | WebSocket (BidiGenerateContent) |
| Client audio plumbing | WebRTC handles mic capture, playback, jitter, echo-cancel natively | Manual: encode mic → PCM16 chunks, decode + schedule playback chunks |
| Server audio proxying | **None** — audio is browser↔OpenAI peer-to-peer | Browser can connect directly (ephemeral token) or server proxies |
| Key exposure | Server mints a short-lived **ephemeral token**; real key never reaches the browser | Ephemeral tokens available; else key would transit the browser (avoid) |
| Server work for MVP | **One handler** (mint ephemeral session) | Token mint OR a WS relay + audio framing |
| PJ's stored key | OpenAI key works (same key; add realtime capability probe) | Google key works (already `voice`-verified) |

**Recommendation: start with OpenAI Realtime, WebRTC, WEB lane.** Why it is the
shorter path to something PJ can actually try:

1. **Least code + least risk on the hard part (audio).** WebRTC's media stream
   abstraction does mic capture, playback, jitter buffering, and echo cancellation
   for us. Gemini Live over WebSocket makes us frame PCM chunks and schedule
   playback by hand — the part most likely to sound broken on the first try.
2. **Cleanest egress boundary for free.** With WebRTC the audio never touches our
   server (browser↔OpenAI direct). Our server sends OpenAI only a session-config
   request — so "only that turn's audio leaves, and it leaves the browser, not us."
3. **Key security is built in.** The ephemeral-token flow keeps the real OpenAI key
   server-side; the browser only ever holds a ~1-minute token.
4. PJ said either is fine and he has a working OpenAI key — so pick the one whose
   first tryable version is smallest. Gemini Live is a fast follow behind the same
   UI once the lane exists (swap the token-mint + client transport).

## 3. What each lane owes

### Server (server-web lane) — small for the MVP
- **`POST /api/voice/session`** (new): gated on a vault credential that verifies the
  realtime capability; mints an OpenAI **ephemeral session token** from the vault
  key (`POST /v1/realtime/client_secrets`, server-side) and returns
  `{token, expires_at, model, voice}`. The real key never leaves the server.
- **Capability probe + catalog**: add realtime to OpenAI's declared kinds and probe
  it (models list contains a `*realtime*` model → capable), and add the `voice`
  kind to `integratedKinds`/`kindOf` so the `voice_provider` selector gates + lights
  up from the stored key. Gemini already declares `voice`.
- **RAG grounding (increment 2, not MVP)**: expose a retrieval tool the realtime
  session can call (function calling) so book answers stay position-/passage-bounded
  server-side — see §4.
- NOT needed for the WebRTC MVP: any audio proxying or WS relay.

### Web (server-web lane) — the real client work
- A **mic/"Talk" control** in the AI panel (the panel is text-only today).
- `getUserMedia` → `RTCPeerConnection`; fetch the ephemeral token from
  `/api/voice/session`; POST the SDP offer to OpenAI's realtime endpoint with the
  token; attach the remote audio track to an `<audio>` element.
- A small **connection state machine** (idle → connecting → live → error), plus
  barge-in (user interrupts playback) and a visible "listening/speaking" indicator.
- Pause the audiobook while a session is live; resume after.

### Mobile (mobile lane) — real work, flagged for them
- OpenAI Realtime over WebRTC needs **`react-native-webrtc`** (a new native dep,
  EAS dev-build required). Alternatively WebSocket + `expo-audio` streaming (more
  manual). Mobile already ships a **Deepgram half-duplex loop** (`AiPanel.tsx`) —
  the realtime lane either replaces it or coexists; **that is a mobile-lane call**,
  not ours. Mobile owes: transport client, mic/playback, the same token fetch from
  `/api/voice/session`, and state machine.
- Mobile is **not blocked on server for the MVP** beyond the session endpoint.

## 4. Egress boundary (privacy stance — enforced, not commented)

- **MVP (ungrounded conversation):** the ONLY thing our server sends the provider is
  the **session-config** (model, voice, generic instructions) — **no book text**.
  The turn's audio leaves the **browser** (not our server) to the provider, and it
  is live mic audio, not library content. Provider policy governs it; we link it and
  stop (per `docs/outbound-data.md`).
  - **Enforcing test (required before shipping):** capture the outbound
    session-create request in `/api/voice/session` and assert its body carries no
    book/library text — only model/voice/instructions. Mirrors
    `TestQAOutboundBoundary_MinimalSend` / `TestEmbedOutboundBoundary_TextOnly`.
- **Grounded (increment 2):** to answer about the book, prefer **function calling**
  over stuffing passages into the prompt — the model calls back to our server for
  retrieval, so only the **retrieved, position-bounded passages** for that question
  leave (the exact Q&A bound), never the whole book. Enforcing test mirrors the Q&A
  boundary test on the retrieval-tool handler.
- Add both to `docs/outbound-data.md` as they land; the doc already reserves the
  "realtime voice" row as a hard gate.

## 5. Credential handling — no second paste

- PJ already has both keys in the vault (OpenAI `[llm,stt,tts]`, Google
  `[llm,voice]`). The `/api/voice/session` handler resolves the provider's key from
  the vault (`store.CredentialAPIKey`, pm34) — **he pastes nothing again**.
- For OpenAI, add a realtime capability probe so the OpenAI credential also lights
  the Voice Chat lane; Google's `voice` is already verified. The gated
  `voice_provider` selector then offers exactly what his stored keys support (same
  pattern as the llm/stt/tts selectors).

## 6. Suggested phasing

1. **Server session endpoint + capability/gating** (small, testable without a UI:
   assert a token is minted from the vault key and the request carries no book text).
2. **Web WebRTC MVP** — mic button → live ungrounded voice with OpenAI. PJ can try it.
3. **RAG grounding via function calling** (bounded egress + test).
4. **Gemini Live** behind the same UI (swap token-mint + transport).
5. **Mobile** realtime client (mobile lane; dep + EAS build).

## 7. Open questions / risks

- WebRTC in the embedded web UI under the relay/tunnel — STUN/ICE reachability
  should be validated early (OpenAI's WebRTC endpoint handles the media path, but
  confirm no corp/NAT surprise on PJ's setup).
- Ephemeral-token endpoint shape/model ids for OpenAI Realtime and Gemini Live
  should be pinned against the live API at build time (model names move; verify, do
  not trust docs — same discipline that caught `gemini-1.5-flash` being retired).
- Cost/latency: realtime audio is metered per minute; surface that in the UI.
- Barge-in / echo handling is where realtime demos usually feel broken — budget
  time for it even though WebRTC helps.

---

# TURN / tunnel reachability — scope + recommendation (2026-07-29)

**Question posed:** the phone-over-tunnel is PJ's primary way of using the product,
so voice that only works on the same Wi-Fi is a demo, not a feature. Add a TURN
relay so it works over the tunnel — but a TURN relay carries media through a
server, and *whose* server has direct privacy consequences.

## Evidence (pinned against the LIVE API, not docs)

Inspected the real OpenAI Realtime WebRTC answer SDP + session response:

- The session mint returns **no `ice_servers`** — OpenAI provides no TURN; the
  client is expected to reach OpenAI's media endpoint itself.
- OpenAI is **ICE-lite** and publishes **public host candidates on BOTH
  `udp:3478` and `tcp:443`** (e.g. `172.214.226.198 443 tcp typ host tcptype
  passive`). OpenAI's media endpoint is directly, publicly routable, and
  **accepts media over TCP/443**.

**Why this reframes the problem:** the tunnel/mobile-data failure is a blocked
**UDP** path. But OpenAI already offers a **TCP/443** media path, which looks like
HTTPS and traverses almost every restrictive network — including a phone on mobile
data behind CGNAT. The browser's ICE agent tries both; if UDP is blocked it should
fall back to OpenAI's TCP/443 candidate. **That path needs no relay at all, and
keeps audio browser↔OpenAI — exactly the privacy stance already committed.**

## The fork

1. **No relay — rely on OpenAI's TCP/443 fallback (RECOMMENDED).** Audio stays
   browser↔OpenAI. Nobody relays it; we carry nothing. Privacy story is unchanged
   and stateable plainly. Already shipped the client robustness for it (widened
   ICE window for slower TCP-ICE; on-connect readout of UDP-vs-TCP so a tunnel
   connection is legible). **Cost: $0, no new infra, no privacy change.**
2. **Abookify-operated TURN — REJECT.** Would route PJ's voice through our servers.
   Directly contradicts the "exhaustive claims about our own handling" stance — we
   would be relaying user audio. Not an option by default.
3. **User self-hosted coturn — REJECT as the default.** Privacy-clean in principle
   (the relay is the user's own box), BUT the target user runs this behind
   home NAT/CGNAT — which is *why they use the tunnel*. A coturn on that box isn't
   reachable from the phone unless it's exposed publicly (most self-hosters can't)
   or tunnelled. It re-creates the reachability problem the tunnel exists to solve.
4. **Third-party TURN (Twilio/Cloudflare/metered) — REJECT as default.** Requires a
   third-party account and routes audio through them. The brief explicitly rules
   out requiring a third-party account by default.

Can NullBore carry TURN? NullBore is an HTTP(S) reverse tunnel; it does not speak
the STUN/TURN protocol, and TURN needs its own reachable listener. Not a fit
without a raw-TCP tunnel feature, and even then it only helps option 3.

## Recommendation

**Do NOT build a TURN relay yet.** The evidence says we most likely don't need one:
OpenAI's own TCP/443 media path should carry the tunnel case with audio staying
browser↔OpenAI. Building a relay now is premature and would introduce the exact
privacy problem the brief flags (us carrying audio) — or an unsolved reachability
problem (coturn behind CGNAT) — for a benefit that may not exist.

**Next step is a measurement, not a build:** have PJ try voice over the tunnel
from his phone. The UI now shows whether it connected over **UDP** or **TCP** (and
a clear message if neither works). Three outcomes:

- **Connects over TCP** → done. No relay ever needed; audio is browser↔OpenAI.
- **Connects over UDP** → also done (his network wasn't blocking UDP after all).
- **Neither** (rare — some carriers block even TCP/443 to non-standard hosts, or
  do deep-packet filtering) → *then* revisit, and the only privacy-clean answer is
  a relay on infrastructure the user controls with a public path (e.g. coturn
  co-located with the NullBore relay, if that relay grows a TCP-tunnel or a
  bundled-TURN feature). We would state plainly, in the UI and the privacy doc,
  that in that fallback mode media transits that relay.

This keeps the default on the path where **no one relays PJ's audio**, and only
escalates to a relay with eyes open if the measurement proves it necessary.
