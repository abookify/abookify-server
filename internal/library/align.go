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
func AlignChapter(store *db.Store, sttClient *stt.Client, workID, audioBookID int64, chapterIdx int, audioPath string, originalText string) error {
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

	// If we have the original text, align Whisper timestamps to it
	var finalTimestamps []db.SyncTimestamp
	if originalText != "" {
		finalTimestamps = AlignTimestampsToSource(originalText, whisperWords)
		log.Printf("align: mapped to %d original words (from %d whisper words)",
			len(finalTimestamps), len(whisperWords))
	} else {
		finalTimestamps = whisperWords
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
