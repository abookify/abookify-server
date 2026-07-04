package server

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"

	"github.com/pj/abookify/internal/applog"
)

// Pre-generated voice previews. Realtime TTS preview was too slow/fragile (it
// "spun forever" on mobile), so each Kokoro voice gets a short sample generated
// ONCE via the TTS service and cached as a static mp3, served at
// GET /api/tts/voices/{voice}/preview.mp3. Bump voicePreviewVersion to force
// regeneration (e.g. if the sample sentence changes); a new voice regenerates
// lazily on first request. Web + mobile share this endpoint.
const (
	voicePreviewVersion  = "v1"
	voicePreviewSentence = "This is a preview of my voice. I can read your books aloud, one word at a time."
)

var voiceNameRe = regexp.MustCompile(`^[a-z]{1,3}_[a-z0-9]+$`)

// knownKokoroVoices is the set the settings UI offers — the authoritative voice
// list. Previews are only generated for these (also guards against generating
// arbitrary input / path traversal).
func knownKokoroVoices() map[string]bool {
	m := make(map[string]bool)
	for _, g := range kokoroVoiceGroups {
		for _, o := range g.Options {
			m[o.Value] = true
		}
	}
	return m
}

func validVoice(v string) bool {
	return voiceNameRe.MatchString(v) && knownKokoroVoices()[v]
}

func (s *Server) voicePreviewDir() string {
	return filepath.Join(s.LibraryDir, "tts-previews")
}

func (s *Server) voicePreviewPath(voice string) string {
	return filepath.Join(s.voicePreviewDir(), voice+"."+voicePreviewVersion+".mp3")
}

// handleVoicePreview serves a voice's pre-generated sample clip, generating +
// caching it on first request (GET /api/tts/voices/{voice}/preview.mp3).
func (s *Server) handleVoicePreview(w http.ResponseWriter, r *http.Request) {
	voice := r.PathValue("voice")
	if !validVoice(voice) {
		http.NotFound(w, r)
		return
	}
	if path := s.voicePreviewPath(voice); fileNonEmpty(path) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
		http.ServeFile(w, r, path)
		return
	}
	audio, err := s.generateVoicePreview(voice)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "voice preview unavailable — the TTS service isn't running",
		})
		return
	}
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
	w.Write(audio)
}

// generateVoicePreview synthesizes + caches a voice's clip. Idempotent (serves
// the cache if another request already produced it). Serialized by a single
// mutex — generation is rare and Kokoro handles one request at a time anyway.
func (s *Server) generateVoicePreview(voice string) ([]byte, error) {
	s.voicePreviewMu.Lock()
	defer s.voicePreviewMu.Unlock()

	path := s.voicePreviewPath(voice)
	if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
		return data, nil
	}
	if s.Generator == nil || s.Generator.TTSClient() == nil {
		return nil, fmt.Errorf("TTS service not available")
	}
	audio, err := s.Generator.TTSClient().Synthesize(voicePreviewSentence, voice)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.voicePreviewDir(), 0o755); err == nil {
		tmp := path + ".tmp"
		if os.WriteFile(tmp, audio, 0o644) == nil {
			os.Rename(tmp, path) // atomic publish
		}
	}
	return audio, nil
}

// prewarmVoicePreviews best-effort generates any missing clips in the background
// so the settings UI shows instant, pre-cached samples. Bails if the TTS service
// isn't up yet — the lazy handler covers it on demand later.
func (s *Server) prewarmVoicePreviews() {
	missing := 0
	for v := range knownKokoroVoices() {
		if fileNonEmpty(s.voicePreviewPath(v)) {
			continue
		}
		if _, err := s.generateVoicePreview(v); err != nil {
			return // TTS not ready — bail; the endpoint regenerates on demand
		}
		missing++
	}
	if missing > 0 {
		applog.Info("server", fmt.Sprintf("voice previews: generated %d clip(s) → %s", missing, s.voicePreviewDir()))
	}
}

func fileNonEmpty(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Size() > 0
}
