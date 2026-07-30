package server

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"

	"github.com/pj/abookify/internal/library"
)

// "Preview a block" — the missing rung in the TTS-trial flow (pick chapter →
// preview text → pick voice → PREVIEW A BLOCK → generate the full edition). The
// older previews (/api/tts/preview, /api/tts/voices/{v}/preview.mp3) synthesize a
// CANNED sentence, so they judge the voice but not how THIS book will actually
// sound. This synthesizes a real block of the chosen chapter through the SAME
// path the full edition uses — PreprocessForTTS → SplitTextForTTS → Synthesize —
// so what the reader auditions is exactly what they'll get.
//
// GET /api/books/{bookId}/chapters/{idx}/tts-preview?voice=af_heart&block=0&words=90
// Returns audio/mpeg; the exact text synthesized is returned (base64) in the
// X-Preview-Text header so the wizard can show precisely what's being read.
const (
	previewBlockDefaultWords = 90  // a paragraph-ish block: representative but quick to synth
	previewBlockMaxWords     = 200 // hard cap so a preview stays a preview
)

func (s *Server) handleChapterTTSPreview(w http.ResponseWriter, r *http.Request) {
	bookID, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("bookId")), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid bookId"})
		return
	}
	idx, err := strconv.Atoi(strings.TrimSpace(r.PathValue("idx")))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid chapter idx"})
		return
	}

	voice := strings.TrimSpace(r.URL.Query().Get("voice"))
	if voice == "" {
		voice = "af_heart"
	}
	if len(voice) > 64 { // a voice id is short; anything longer is bogus input
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid voice"})
		return
	}
	words := previewBlockDefaultWords
	if q := strings.TrimSpace(r.URL.Query().Get("words")); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 {
			words = n
		}
	}
	if words > previewBlockMaxWords {
		words = previewBlockMaxWords
	}
	block := 0
	if q := strings.TrimSpace(r.URL.Query().Get("block")); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n >= 0 {
			block = n
		}
	}

	if s.Generator == nil || s.Generator.TTSClient() == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "TTS service not available — start it or configure a cloud TTS provider in Settings",
		})
		return
	}

	ch, err := s.store.GetChapterContent(bookID, idx)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	if ch == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "chapter not found"})
		return
	}

	// Identical preprocessing + chunking to the full-edition path, so the preview
	// is representative of the real narration.
	processed := library.PreprocessForTTS(ch.Title, ch.Content)
	blocks := library.SplitTextForTTS(processed, words)
	if len(blocks) == 0 || strings.TrimSpace(processed) == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "this chapter has no narratable text to preview",
		})
		return
	}
	if block >= len(blocks) {
		block = len(blocks) - 1 // clamp: asking past the end previews the last block
	}
	text := blocks[block]

	audio, err := s.Generator.TTSClient().Synthesize(text, voice)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "synthesis failed: " + err.Error()})
		return
	}

	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Cache-Control", "no-store") // previews vary by voice/block; don't cache
	w.Header().Set("X-Preview-Text", base64.StdEncoding.EncodeToString([]byte(text)))
	w.Header().Set("X-Preview-Block", strconv.Itoa(block))
	w.Header().Set("X-Preview-Total-Blocks", strconv.Itoa(len(blocks)))
	// Let a cross-origin caller (mobile over the tunnel) read the preview headers.
	w.Header().Set("Access-Control-Expose-Headers", "X-Preview-Text, X-Preview-Block, X-Preview-Total-Blocks")
	w.WriteHeader(http.StatusOK)
	w.Write(audio)
}
