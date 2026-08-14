// Content-addressed TTS audio store + resumable working directory (board
// task 14, PJ-approved, pre-launch).
//
// THE DEFECT THIS REMOVES: generated audio was keyed by IDENTITY
// (tts-book-<id>-<voice>/chapter-N.mp3, skip-if-exists), so the cache key
// did not include the TEXT. Change a chapter's words and the generator
// silently served the old narration, with word timings that mistime against
// text that no longer matches — and invalidation was manual (regenerating
// Carol meant deleting the edition by hand). Same lesson as edition
// identity, one layer down: key on what a thing IS (the words it speaks +
// the voice + the cadence), not where it sits.
//
// LAYOUT (both under the generator's own dir — the scanner never walks it,
// so neither is ever discovered or served):
//   <generated>/cas/<hh>/<hash>.mp3   immutable finished chapter audio
//   <generated>/work/<job-id>/        the in-progress chapter's assembly
// A chapter file in an edition dir is a HARDLINK to its CAS entry, with a
// tiny sidecar (chapter-N.mp3.cas) recording the key. "Done" means the
// sidecar key MATCHES the current content key — not merely that a file
// exists. Resume granularity stays CHAPTER (extending the existing
// skip-if-exists resume, not duplicating it): a crash costs only the
// chapter in flight, and a re-run after a text change re-synthesizes
// exactly the chapters whose words changed.
//
// Promotion is atomic: the chapter assembles in work/, os.Rename()s into
// the CAS (same filesystem — the working dir lives under the data dir, NOT
// /tmp, which may be tmpfs or tiny), then links into the edition. Half a
// chapter can never be indistinguishable from a finished one (task 12's
// rule in the file layer).
//
// GC SHIPS WITH THE FEATURE: CleanTTSCas removes CAS entries no edition
// links to (st_nlink==1) after a grace period, and work/ dirs whose job is
// gone. Wasting disk beats playing the wrong narration; unbounded waste on
// a user's machine is still our bug.
package library

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// ttsCadenceVersion bumps whenever synthesis semantics change in a way that
// should invalidate cached audio (pause insertion, preprocessing rewrites).
const ttsCadenceVersion = "cadence-v2-paused"

// TTSContentKey is the content address of one chapter's audio: the words it
// speaks (whitespace-normalized), the voice, and the cadence parameters.
func TTSContentKey(processedText, voice string, titlePauseMs, paraPauseMs int) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%s\n%d,%d\n", ttsCadenceVersion, voice, titlePauseMs, paraPauseMs)
	h.Write([]byte(strings.Join(strings.Fields(processedText), " ")))
	return hex.EncodeToString(h.Sum(nil))
}

func casPath(generatedDir, key string) string {
	return filepath.Join(generatedDir, "cas", key[:2], key+".mp3")
}

// CasHasChapter reports whether the edition file at mp3Path is current for
// key: the file exists AND its sidecar records the same key.
func CasHasChapter(mp3Path, key string) bool {
	if _, err := os.Stat(mp3Path); err != nil {
		return false
	}
	b, err := os.ReadFile(mp3Path + ".cas")
	return err == nil && strings.TrimSpace(string(b)) == key
}

// CasLinkChapter materializes the CAS entry for key at mp3Path (hardlink,
// copy fallback) and stamps the sidecar. Returns false if the CAS has no
// entry for key.
func CasLinkChapter(generatedDir, key, mp3Path string) bool {
	src := casPath(generatedDir, key)
	if _, err := os.Stat(src); err != nil {
		return false
	}
	os.Remove(mp3Path)
	if err := os.Link(src, mp3Path); err != nil {
		data, rerr := os.ReadFile(src)
		if rerr != nil || os.WriteFile(mp3Path, data, 0644) != nil {
			return false
		}
	}
	if err := os.WriteFile(mp3Path+".cas", []byte(key+"\n"), 0644); err != nil {
		log.Printf("tts-cas: sidecar write failed for %s: %v", mp3Path, err)
	}
	return true
}

// CasWorkDir returns (creating) the working directory for a job — under the
// generator dir, never scanned, never served.
func CasWorkDir(generatedDir, jobID string) (string, error) {
	d := filepath.Join(generatedDir, "work", jobID)
	return d, os.MkdirAll(d, 0755)
}

// CasPromote atomically moves a finished chapter assembly into the CAS
// under key and links it to mp3Path. workFile must be on the same
// filesystem as the CAS (both live under generatedDir).
func CasPromote(generatedDir, key, workFile, mp3Path string) error {
	dst := casPath(generatedDir, key)
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	if err := os.Rename(workFile, dst); err != nil {
		return fmt.Errorf("cas promote: %w", err)
	}
	if !CasLinkChapter(generatedDir, key, mp3Path) {
		return fmt.Errorf("cas link-after-promote failed for %s", mp3Path)
	}
	return nil
}

// CleanTTSCas is the GC sweep: unreferenced CAS entries (nlink==1) older
// than grace are removed; work dirs not in keepJobs are removed. Returns
// (entries removed, work dirs removed).
func CleanTTSCas(generatedDir string, grace time.Duration, keepJobs map[string]bool) (int, int) {
	removed := 0
	casRoot := filepath.Join(generatedDir, "cas")
	filepath.WalkDir(casRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".mp3") {
			return nil
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil
		}
		nlink := uint64(1)
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			nlink = uint64(st.Nlink)
		}
		if nlink <= 1 && time.Since(info.ModTime()) > grace {
			if os.Remove(path) == nil {
				removed++
			}
		}
		return nil
	})
	workRemoved := 0
	entries, _ := os.ReadDir(filepath.Join(generatedDir, "work"))
	for _, e := range entries {
		if !e.IsDir() || keepJobs[e.Name()] {
			continue
		}
		if os.RemoveAll(filepath.Join(generatedDir, "work", e.Name())) == nil {
			workRemoved++
		}
	}
	return removed, workRemoved
}
