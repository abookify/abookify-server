# Outbound data — what leaves this server, per feature

This is the exhaustive enumeration of every call abookify makes to a third-party
provider, and precisely what payload leaves. It describes **our own behavior**
only. It makes **no claim** about what any provider does with the data — each
provider's own policy governs that; we link it and stop.

The point is that the boundary is **enforced in code**, not just described here:
each row names the test or code comment that holds the line, so a future change
that widens a payload fails a test rather than silently over-sending.

Nothing leaves the machine unless you configure a **cloud** provider for a
feature. With the local engines (Kokoro TTS, Whisper STT) and no LLM key, every
call below is to `localhost` and no data leaves the host.

| Feature | Outbound call | Exactly what leaves | Never sent | Enforced by |
|---|---|---|---|---|
| **Book Q&A** (chat / `/ask` / voice converse) | LLM chat-completions | The **retrieved, position-bounded passages** + your question | The rest of the book; un-retrieved chunks; other works | `qa.go` OUTBOUND-DATA BOUNDARY comment + `TestQAOutboundBoundary_MinimalSend` (capture server asserts non-retrieved book text never leaves) |
| **RAG index** (embeddings, built when you enable an LLM) | LLM embeddings | The **chunk texts** to embed + the model id | Title, author, file path, any library metadata | `embed.go` OUTBOUND-DATA BOUNDARY comment + `TestEmbedOutboundBoundary_TextOnly` (asserts only `input`+`model` keys leave) |
| **Cloud TTS** (only if `tts_provider=openai`) | `/v1/audio/speech` | The **text span** to narrate + voice/model/format | Title, author, path, library metadata | `tts/openai.go` OUTBOUND-DATA BOUNDARY comment |
| **Cloud STT** (only if `stt_provider=openai`) | `/v1/audio/transcriptions` | The **audio file** to transcribe + model/format/granularity flags (form filename is the bare basename) | Title, author, full path, library metadata | `stt/openai.go` OUTBOUND-DATA BOUNDARY comment |
| **Capability probe / Test connection** | Provider model-list endpoint | Your **API key** (to the vendor it belongs to), to verify it | Any book content | `provider_probe.go` — a bare model-list GET, no content |

## Extract-only Q&A

When **Answer only from the book text** (`qa_extract_only`) is on, Book Q&A makes
**no LLM call at all** — it returns the retrieved, position-bounded passages
verbatim. In that mode the "Book Q&A" row above sends **nothing** to any
provider (it works with no LLM key configured). See `composeExtractAnswer` in
`qa.go`.

## Local engines send to localhost only

- **Kokoro TTS** (`tts/client.go`) and **Whisper STT** (`stt/client.go`) POST to
  the configured local URL (default `localhost:8880` / `localhost:5200`). The
  same minimal payload applies (text span / audio file), but it does not leave
  the host.
- **Ollama** embeddings/LLM (`embed.go`, `llm.go`) likewise go to a local
  endpoint by default.

## Provider policies (theirs, not ours)

We link each provider's own policy and do not characterize it. See
`ProviderDescriptor.PolicyURL` in `provider_catalog.go`:

- OpenAI — <https://openai.com/policies/privacy-policy/>
- Google (Gemini) — <https://policies.google.com/privacy>
- Anthropic — <https://www.anthropic.com/legal/privacy>

Their terms govern what happens to anything sent above.

## Not yet built

- **Voice conversation** (speech-to-speech; Gemini Live / OpenAI Realtime /
  Deepgram) is not wired as an egress path yet. When it lands it must send **only
  that turn's audio** and be added here with an enforcing test before shipping.
