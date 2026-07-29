# Settings restructure — credentials + per-feature provider & model

Status: **PLAN (approved shape, awaiting go-ahead to build)** · 2026-07-29 · owner: server-web

## What PJ approved

- A **Keys** section: the user adds credentials for various services once. It must
  be usable **on its own** — a key can be added before anything consumes it (PJ
  will add Gemini + OpenAI keys before the voice path exists). An added-but-unused
  key is fine; a key entry that's *blocked* on an unfinished feature is not.
- Per-feature settings (Q&A / STT / TTS) then choose **provider AND model** — pick
  the model, not just the vendor (Q&A: cheaper vs stronger; STT: which
  transcription model; TTS: which voice/tier).
- Models per provider are served by the **backend #202 schema** (web + mobile
  render the same list; **no hardcoded model names in the UI**).

## Feature lanes / provider "kinds"

A provider declares one or more **kinds**; the kind decides which selector it
appears in. This keeps speech-to-speech out of the wrong category:

| Kind | Feature | Constraint |
|---|---|---|
| `llm` | Q&A / summaries | provider + model |
| `stt` | transcription for **karaoke** | **word-timestamp GATE** (see below) |
| `tts` | narration | **ranked, not gated** (see below) |
| `voice` | **live voice conversation** (speech-to-speech) | separate lane; NOT an STT or TTS selector |

**Speech-to-speech (Gemini Live, OpenAI Realtime, Deepgram-for-voice) are `voice`
kind only.** They skip the STT→LLM→TTS round-trip, so they must never appear in
the STT or TTS provider dropdowns. Voice is the last lane built (after e2e).

## Constraints (locked)

1. **Forward-compatible data model, no forward UI.** Model credentials as
   **provider → credentials (one-to-many capable)**, even though the UI exposes
   exactly ONE key per provider today. Costs nothing now; avoids a migration if
   per-lane cost tracking is ever wanted. **Do NOT build any multi-key UI.**
2. **Providers declare their own credential fields.** OpenAI = `{api_key}`, Azure
   OpenAI = `{api_key, region, deployment}`, AWS = `{access_key_id,
   secret_access_key, region}`. So the Keys section is data-driven, not hardcoded.
3. **Migrate existing keys with zero re-entry.** `llm_api_key`, `stt_api_key`,
   `voice_api_key` (+ their `*_provider`/`*_model`) must carry over silently.
4. **Scope pullback: wire + genuinely VERIFY one cloud provider end-to-end**
   (OpenAI — PJ already has a working key). Do not go wide.
5. **STT word-timestamp GATE (karaoke STT only).** A karaoke work's STT
   provider/model must return per-word timings at the fidelity the anchor aligner
   needs; ones that can't are **not selectable** for karaoke and the UI says why.
   Verify per provider against the real aligner, never assume from docs. (This is
   the one constrained lane — because there's no post-hoc recovery for STT.)
6. **TTS is RANKED, not gated.** Our pipeline recovers timings after the fact
   (Whisper over the generated audio → map back), so **any** TTS provider works.
   Providers with **native** word/char timings (ElevenLabs, Azure word-boundary,
   Polly speech marks, Google timepoints) let us **skip** that pass — faster and
   more accurate — so surface them as a **recommendation with the reason shown**
   ("gives word timings directly, so sync is exact and generation is faster"),
   **without hiding the others**. Model TTS providers in two classes (native-timing
   vs needs-alignment-pass) and let the pipeline branch on it.
7. **Deepgram: voice-conversation ONLY** (narrow, testable). NOT an STT-for-karaoke
   provider — that sidesteps the word-timestamp fidelity risk entirely. Deepgram
   was never implemented (its only prior mention was a grammar comment in the LLM
   note); it enters as a `voice`-kind provider when the voice lane is built.
8. **No-GPU pairing is coherent.** A user without a 12 GB GPU can't run Whisper
   large-v3, so cloud TTS implies cloud STT (the alignment pass has to run
   somewhere). Make that combination a supported configuration, not an accident.
9. **One entry per VENDOR; features light up from the credential's verified
   capabilities — never assumed.** Vendors issue account-level keys (Google gives
   one key covering Gemini LLM, Gemini Live voice, Gemini TTS), so the Keys
   section is keyed per **vendor**: one Google key lights up Gemini across every
   lane it serves, with no re-paste. BUT one key does **not** unlock everything a
   vendor sells — a Gemini key does not necessarily reach **Google Cloud TTS**
   (WaveNet/Neural2), a separate GCP product needing project enablement. So each
   credential **declares the capabilities it actually satisfies** (probed/verified
   on save, stored on the row), and per-feature selectors offer only those —
   never the full vendor menu that then fails at call time. **Verify Google TTS
   reachability from a Gemini key before wiring anything Google-TTS.** Same logic
   protects Azure/AWS (which need multi-field credentials anyway). Getting this
   entry form right matters more right now than any single provider integration.

## Data model

New table (additive; nothing else changes shape):

```sql
CREATE TABLE credentials (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  provider     TEXT NOT NULL,             -- the VENDOR: 'openai' | 'google' | 'azure_openai' | 'aws' | 'anthropic' | …
  label        TEXT NOT NULL DEFAULT '',  -- '' today; names the key in a future multi-key UI
  fields       TEXT NOT NULL DEFAULT '{}',-- JSON: {"api_key":…} | {"api_key":…,"region":…,"deployment":…} | …
  capabilities TEXT NOT NULL DEFAULT '[]',-- JSON array of VERIFIED feature kinds, e.g. ["llm","voice"]
  created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_credentials_provider ON credentials(provider);
-- NO UNIQUE(provider): one-to-many is allowed at the DB level; the UI/migration
-- create at most one row per vendor today. `capabilities` is what THIS key was
-- probed to satisfy (a subset of what the vendor sells) — per-feature selectors
-- gate on it. Store methods: Create/Get/List/UpsertProviderCredential/
-- SetCredentialCapabilities/DeleteCredential (internal/db/db.go, tested).
```

Per-feature choice stays in the `settings` KV — these are per-feature, not
credentials: `llm_provider`, `llm_model`, **`llm_credential_id`** (new; defaults
to the provider's sole credential), and the same triple for `stt_*` and `tts_*`
(voice), and later `voice_*`. `credential_id` is the seam that makes multi-key
possible later without a schema change; today it points at the one row for that
provider. A credential can exist with **no** feature pointing at it (the
add-a-key-before-it's-consumed case).

Secret handling: the `fields` blob's secret sub-keys are masked on read the same
way `isSecretSettingKey` masks `*_api_key`/`*_token` today (extend the masker to
the credential fields a provider marks `secret`).

## Provider registry (Go, code — not DB)

One source of truth the schema + providers read from:

```go
type CredentialField struct { Key, Label, Placeholder string; Secret, Required bool }
type Model struct {
    ID, Label string
    WordTimestamps bool // STT: gate;  TTS: native-timing → "recommended" ranking
    Tier string         // TTS voice/quality tier
}
type ProviderDescriptor struct {
    ID, Label string
    Kinds     []string          // "llm" | "stt" | "tts" | "voice" (several allowed)
    Credential []CredentialField // what the Keys section renders for this provider
    Models    map[string][]Model // per-kind model catalog (served via OptionsEndpoint)
}
```

- Drives the **Keys** section (credential fields) and the per-feature **model**
  lists — both through the existing #202 `OptionsEndpoint` + `DependsOn` pattern
  (already how `/api/llm/models` works). New siblings: `/api/stt/models`,
  `/api/tts/models`, each keyed by the chosen provider.
- **STT gate:** karaoke filters STT models to `WordTimestamps==true`; the rest
  show disabled with the reason.
- **TTS rank:** native-timing models sort first with a "recommended — exact sync,
  faster" badge; all others remain selectable.

## Migration (additive, non-destructive, boot-time)

If `credentials` is empty and legacy keys exist: for each feature's
`(provider, api_key)`, upsert a credential row per **distinct** `(provider, key)`
and set that feature's `*_credential_id`. Same key across features collapses to
one row. **Leave the legacy `*_api_key` settings in place** as a fallback until
the new resolution path is verified — so nothing is lost and the step is
reversible. No user re-entry.

## Provider resolution

`providers.go` `CreateTTS/STTProvider` (and the LLM client) resolve the key from
`*_credential_id` → `credentials.fields`, falling back to the legacy `*_api_key`
setting when no credential is wired yet. One code path, old installs keep working.

## One-provider end-to-end verification (OpenAI)

Configure the OpenAI credential once in Keys, then prove each lane:
- **Q&A** (LLM): model pick (gpt-4o vs gpt-4o-mini) + Test connection (green today).
- **TTS**: tts-1 voices → generated audio → our existing STT-alignment pass maps
  timings back (TTS unconstrained; OpenAI TTS has no native timing → needs the pass).
- **STT**: transcription model + **word-timestamp capability PROVEN against the
  anchor aligner** on a known book — report real accuracy + speed vs local
  Whisper large-v3 so PJ can judge; if it can't match, gate it out of karaoke and
  say so plainly rather than shipping drifting sync.

## Commit sequence (each discrete)

1. `credentials` table + Store methods (additive; no behavior change).
2. Provider descriptor registry (credential fields + per-kind models + STT
   word-timestamp gate flag + TTS native-timing rank flag + `kinds` incl. `voice`).
3. Non-destructive boot migration (legacy keys → credentials, keep fallback).
4. #202 schema: **Keys** section rendered from the registry (masked fields),
   usable standalone.
5. #202 schema: per-feature provider + model selectors; `/api/stt|tts/models`;
   STT gate + TTS recommendation surfaced.
6. Resolve providers from credentials (fallback to legacy settings).
7. OpenAI end-to-end + STT word-timestamp verification + written report.

Voice lane (Deepgram / Gemini Live / OpenAI Realtime) is a **later** lane, after
step 7 — scoped separately as speech-to-speech, not through the STT/TTS surfaces.
Steps 1–4 already let PJ add OpenAI + Gemini keys (unconsumed is fine).
