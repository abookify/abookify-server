package library

import (
	"encoding/json"
	"fmt"
	"log"

	"github.com/pj/abookify/internal/db"
	"github.com/pj/abookify/internal/stt"
)

// AlignChapter runs Whisper on an audio file to extract word-level timestamps,
// then aligns them back to the original source text so the karaoke display
// shows the real ebook text, not Whisper's interpretation.
//
// If originalText is provided, timestamps are mapped to the original words.
// If empty, raw Whisper timestamps are stored as-is (fallback).
func AlignChapter(store *db.Store, sttClient stt.Provider, workID, audioBookID int64, chapterIdx int, audioPath string, originalText string) error {
	if sttClient == nil {
		return fmt.Errorf("STT service not available")
	}

	log.Printf("align: transcribing %s for word timestamps", audioPath)

	result, err := sttClient.TranscribeFile(audioPath)
	if err != nil {
		return fmt.Errorf("transcribe: %w", err)
	}

	// Extract word-level timestamps from Whisper
	var whisperWords []db.SyncTimestamp

	for _, seg := range result.Segments {
		for _, w := range seg.Words {
			whisperWords = append(whisperWords, db.SyncTimestamp{
				Start: w.Start,
				End:   w.End,
				Word:  w.Word,
			})
		}
	}

	if len(whisperWords) == 0 {
		return fmt.Errorf("no word timestamps extracted")
	}

	log.Printf("align: got %d whisper words (%.1fs audio)", len(whisperWords), result.Duration)

	// Refuse to store a word map for a partial transcription. Whisper's
	// decoder stops at the first malformed frame of a byte-concatenated MP3
	// while player decoders skip it and keep going — so a truncated round-trip
	// used to slip through here, and AlignTimestampsToSource then compressed
	// EVERY original word into the transcribed span: karaoke that drifts off
	// the audio within minutes, stored silently (Gulag chapter-005: 178s
	// transcribed of 1757s, all 4,523 words mapped into it). No sync row is
	// better than a wrong one — absence shows up in every completeness sweep,
	// compression showed up in none.
	if probed := probeDurationFile(audioPath); probed > 60 {
		extent := result.Duration
		if last := whisperWords[len(whisperWords)-1].End; last > extent {
			extent = last
		}
		if extent < 0.85*probed {
			return fmt.Errorf("whisper transcribed only %.0fs of a %.0fs file — refusing to "+
				"store a compressed word map (malformed audio stream? re-encode the file)",
				extent, probed)
		}
	}

	// If we have the original text, align Whisper timestamps to it
	var finalTimestamps []db.SyncTimestamp
	if originalText != "" {
		finalTimestamps = AlignTimestampsToSource(originalText, whisperWords)
		log.Printf("align: mapped to %d original words (from %d whisper words)",
			len(finalTimestamps), len(whisperWords))
	} else {
		finalTimestamps = whisperWords
	}

	// The MAPPED extent gets the same refusal as the transcribed extent: the
	// greedy mapper once emitted every original word compressed into half the
	// narration FROM A COMPLETE TRANSCRIPTION (Alice ch3: 1,702 words ending
	// 294s of 554s), so checking whisper's extent alone is not enough.
	if n := len(finalTimestamps); n > 0 {
		if wEnd := whisperWords[len(whisperWords)-1].End; wEnd > 60 {
			if mEnd := finalTimestamps[n-1].End; mEnd < 0.85*wEnd {
				return fmt.Errorf("mapped word timeline ends at %.0fs of %.0fs transcribed — "+
					"refusing to store a compressed word map (text/transcript mismatch?)",
					mEnd, wEnd)
			}
		}
	}

	// Serialize to compact JSON
	data, err := json.Marshal(finalTimestamps)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	if err := store.SaveSyncData(workID, audioBookID, chapterIdx, string(data)); err != nil {
		return fmt.Errorf("save: %w", err)
	}

	log.Printf("align: stored %d word timestamps for chapter %d", len(finalTimestamps), chapterIdx)

	return nil
}
