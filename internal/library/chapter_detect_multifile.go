// Cross-file chapter detection for multi-file audiobooks whose file names
// don't reveal chapter structure (e.g. 01.mp3, 02.mp3 as 10 equal-size
// sections of Crime and Punishment). The narrator still says "Chapter N"
// in the audio — we just have to look across file boundaries to find them.
//
// Single-file detection (#58) and filename-based linking (linker.go) handle
// the common cases. This fills the gap for section-split audiobooks.
package library

import (
	"encoding/json"
	"log"

	"github.com/pj/abookify/internal/db"
)

// fileSlice is one audio file's contribution to the concatenated timeline:
// its word stream with timestamps already shifted by `baseOffset` so the
// aggregated stream has a continuous global time axis.
type fileSlice struct {
	book       db.Book
	baseOffset float64 // cumulative seconds from start of work
	words      []db.SyncTimestamp
}

// DetectChaptersMultiFile runs chapter detection across all audio files of
// a multi-file work. If any chapters are detected, they're stored as
// Chapter rows on the appropriate audio books (the file where each chapter
// starts), with start_sec as the offset within that file.
//
// Returns the number of detected chapters written.
func DetectChaptersMultiFile(store *db.Store, workID int64) (int, error) {
	work, err := store.GetWork(workID)
	if err != nil || work == nil {
		return 0, err
	}
	if len(work.AudioFiles) < 2 {
		return 0, nil // not multi-file; single-file path handles it
	}

	// Collect sync_data per file, computing cumulative offsets.
	var slices []fileSlice
	var combined []db.SyncTimestamp
	var cumOffset float64
	for i, af := range work.AudioFiles {
		raw, err := store.GetSyncData(workID, af.ID, i)
		if err != nil || raw == "" {
			// No sync data for this file — skip and advance offset by duration.
			cumOffset += af.Duration
			continue
		}
		var fileWords []db.SyncTimestamp
		if err := json.Unmarshal([]byte(raw), &fileWords); err != nil {
			cumOffset += af.Duration
			continue
		}
		// Shift timestamps into the global timeline.
		shifted := make([]db.SyncTimestamp, len(fileWords))
		for j, w := range fileWords {
			shifted[j] = db.SyncTimestamp{
				Start: w.Start + cumOffset,
				End:   w.End + cumOffset,
				Word:  w.Word,
			}
		}
		slices = append(slices, fileSlice{book: af, baseOffset: cumOffset, words: shifted})
		combined = append(combined, shifted...)

		// Advance by actual duration if known, else by last word's end time.
		fileDur := af.Duration
		if fileDur == 0 && len(fileWords) > 0 {
			fileDur = fileWords[len(fileWords)-1].End
		}
		cumOffset += fileDur
	}

	if len(combined) == 0 {
		return 0, nil
	}

	detected := DetectChapters(combined, cumOffset)
	if len(detected) == 0 {
		return 0, nil
	}

	// Group detected chapters by which file they start in, then write.
	written := 0
	for _, ch := range detected {
		idx, localStart, localEnd := locateChapterInFiles(slices, ch.StartSec, ch.EndSec)
		if idx < 0 {
			continue
		}
		book := slices[idx].book
		// Clear any previously-detected chapters for this book on first write.
		store.DeleteChaptersByBook(book.ID)
		store.InsertChapter(db.Chapter{
			BookID:     book.ID,
			Index:      ch.Index,
			Title:      ch.Title,
			Src:        "detected",
			StartSec:   localStart,
			EndSec:     localEnd,
			Confidence: ch.Confidence,
		})
		written++
	}

	log.Printf("chapter-detect-multifile: %d chapters across %d files for work %d",
		written, len(slices), workID)
	return written, nil
}

// locateChapterInFiles finds which audio file a chapter start falls in, and
// computes its file-local start/end seconds. Returns (fileIdx, localStart,
// localEnd). If the chapter extends past the start file, localEnd is clamped
// to the file's end.
func locateChapterInFiles(slices []fileSlice, globalStart, globalEnd float64) (int, float64, float64) {
	for i, s := range slices {
		fileEnd := s.baseOffset
		if len(s.words) > 0 {
			fileEnd = s.words[len(s.words)-1].End
		} else if s.book.Duration > 0 {
			fileEnd = s.baseOffset + s.book.Duration
		}
		if globalStart >= s.baseOffset && globalStart < fileEnd {
			localStart := globalStart - s.baseOffset
			localEnd := globalEnd - s.baseOffset
			// Clamp to file duration if chapter spans beyond this file.
			if fileEnd-s.baseOffset < localEnd {
				localEnd = fileEnd - s.baseOffset
			}
			return i, localStart, localEnd
		}
	}
	return -1, 0, 0
}
