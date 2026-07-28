// split-chapter splits one over-long chapter into two at a text/time boundary.
//
// The motivating case: publisher bonus content. An audiobook's final narrated
// chapter is frequently followed — with no chapter announcement — by an
// interview, an author talk, or a "hopes you enjoyed this program" outro. The
// narrator-pattern detector has nothing to fire on, so all of it fuses onto the
// end of the real last chapter (The Da Vinci Code: 92.7 min against a ~9.5 min
// average, ~64 min of which is not the novel).
//
// Splitting is a DB edit rather than a detector change on purpose: where the
// book ends is an editorial judgement about a specific recording, not a pattern
// worth guessing at. This tool makes that judgement explicit, reviewable, and
// re-runnable — the split otherwise lives only as hand-typed SQL and is lost
// the next time the sidecar is reimported.
//
// The boundary is given as a PHRASE (the last words of the part that stays),
// so the intent survives a re-transcription that shifts word offsets slightly.
// Times come from the work's word-level sync data, so audio and text agree.
//
// Usage (via Docker, no host Go):
//
//	docker run --rm -v "$(pwd)":/app -w /app golang:1.24-bookworm \
//	  go run -buildvcs=false ./cmd/split-chapter \
//	    -db ./data/abookify.db -work 114 -book 121502 -index 105 \
//	    -after "chasms of the earth." -title "Bonus Material" -dry-run
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/pj/abookify/internal/db"
	_ "modernc.org/sqlite"
)

// openDB mirrors db.Open's DSN + single-connection policy (WAL, 15s
// busy_timeout, SetMaxOpenConns(1)) so this tool is safe to run against a live
// server, but skips the migration pass — a maintenance tool has no business
// migrating the schema out from under a running server.
func openDB(path string) (*sql.DB, error) {
	sq, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(15000)")
	if err != nil {
		return nil, err
	}
	sq.SetMaxOpenConns(1)
	return sq, sq.Ping()
}

func main() {
	dbPath := flag.String("db", "./data/abookify.db", "path to the SQLite database")
	workID := flag.Int64("work", 0, "work id (for the word-level sync lookup)")
	bookID := flag.Int64("book", 0, "book id holding the chapter to split")
	index := flag.Int("index", -1, "index_num of the chapter to split")
	after := flag.String("after", "", "boundary phrase — the LAST words kept in the original chapter")
	title := flag.String("title", "", "title for the new second chapter")
	dryRun := flag.Bool("dry-run", false, "report the split without writing")
	flag.Parse()

	if *bookID == 0 || *index < 0 || *after == "" || *title == "" {
		log.Fatal("-book, -index, -after and -title are required")
	}

	sq, err := openDB(*dbPath)
	if err != nil {
		log.Fatalf("open db %s: %v", *dbPath, err)
	}
	defer sq.Close()

	ch, err := loadChapter(sq, *bookID, *index)
	if err != nil {
		log.Fatalf("load chapter: %v", err)
	}
	fmt.Printf("chapter %d of book %d: %q\n  %.1f–%.1fs (%.1f min), %d words, %d chars of content\n",
		ch.Index, ch.BookID, trunc(ch.Title, 60), ch.StartSec, ch.EndSec,
		(ch.EndSec-ch.StartSec)/60, ch.WordCount, len(ch.Content))

	// Boundary times come from the work's word stream, so the audio-side and
	// text-side copies of a chapter land on the same instant.
	endSec, startSec, err := boundaryTimes(sq, *workID, *after, ch.StartSec, ch.EndSec)
	if err != nil {
		log.Fatalf("locate boundary in sync data: %v", err)
	}
	fmt.Printf("boundary: part 1 ends %.2fs, part 2 starts %.2fs (%.1f min moved out)\n",
		endSec, startSec, (ch.EndSec-startSec)/60)

	// Text split. Audio-side chapters carry no content (they are navigation
	// entries), so an empty content column is expected, not an error.
	var head, tail string
	if strings.TrimSpace(ch.Content) != "" {
		head, tail, err = splitContent(ch.Content, *after)
		if err != nil {
			log.Fatalf("split content: %v", err)
		}
		fmt.Printf("content: %d words kept, %d words moved to the new chapter\n",
			wordCount(head), wordCount(tail))
		fmt.Printf("  ...%s\n  >>> %s...\n", tail1(head, 70), trunc(strings.TrimSpace(tail), 70))
	} else {
		fmt.Println("content: none (navigation-only chapter) — splitting times/title only")
	}

	if *dryRun {
		fmt.Println("\ndry-run: nothing written")
		return
	}

	if err := applySplit(sq, *workID, ch, head, tail, endSec, startSec, *title); err != nil {
		log.Fatalf("apply split: %v", err)
	}
	fmt.Printf("\nsplit applied: book %d now has chapter %d + new chapter %d %q\n",
		ch.BookID, ch.Index, ch.Index+1, *title)
}

func loadChapter(sq *sql.DB, bookID int64, index int) (*db.Chapter, error) {
	var c db.Chapter
	err := sq.QueryRow(`SELECT id, book_id, index_num, title, content, word_count, start_sec, end_sec
	                    FROM chapters WHERE book_id = ? AND index_num = ?`, bookID, index).
		Scan(&c.ID, &c.BookID, &c.Index, &c.Title, &c.Content, &c.WordCount, &c.StartSec, &c.EndSec)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("no chapter %d in book %d", index, bookID)
	}
	return &c, err
}

var wordRe = regexp.MustCompile(`[a-z0-9]+`)

func norm(s string) []string { return wordRe.FindAllString(strings.ToLower(s), -1) }

func wordCount(s string) int { return len(strings.Fields(s)) }

// boundaryTimes finds the phrase in the work's book-continuous word timestamps
// and returns (end of the last phrase word, start of the following word). The
// gap between them is dead air at the seam — typically several seconds of
// silence before bonus content begins.
func boundaryTimes(sq *sql.DB, workID int64, phrase string, lo, hi float64) (float64, float64, error) {
	if workID == 0 {
		return 0, 0, fmt.Errorf("-work is required to resolve boundary times")
	}
	words, err := loadSyncWords(sq, workID)
	if err != nil {
		return 0, 0, err
	}
	target := norm(phrase)
	if len(target) == 0 {
		return 0, 0, fmt.Errorf("boundary phrase has no searchable words")
	}

	var hits []int
	for i := 0; i+len(target) <= len(words); i++ {
		// Only consider matches inside the chapter being split — a short
		// phrase can easily recur elsewhere in a long book.
		if words[i].Start < lo || words[i].Start > hi {
			continue
		}
		ok := true
		for j, t := range target {
			w := norm(words[i+j].Word)
			if len(w) != 1 || w[0] != t {
				ok = false
				break
			}
		}
		if ok {
			hits = append(hits, i)
		}
	}
	switch {
	case len(hits) == 0:
		return 0, 0, fmt.Errorf("phrase %q not found in the chapter's word stream", phrase)
	case len(hits) > 1:
		return 0, 0, fmt.Errorf("phrase %q is ambiguous — %d matches in this chapter; use a longer phrase",
			phrase, len(hits))
	}

	last := hits[0] + len(target) - 1
	if last+1 >= len(words) {
		return 0, 0, fmt.Errorf("phrase %q ends the book — nothing to split off", phrase)
	}
	return words[last].End, words[last+1].Start, nil
}

type syncWord struct {
	Start float64 `json:"s"`
	End   float64 `json:"e"`
	Word  string  `json:"w"`
}

func loadSyncWords(sq *sql.DB, workID int64) ([]syncWord, error) {
	rows, err := sq.Query(`SELECT timestamps FROM sync_data WHERE work_id = ? ORDER BY chapter_idx`, workID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var all []syncWord
	var blobs []string
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		blobs = append(blobs, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, b := range blobs {
		var ws []syncWord
		if err := json.Unmarshal([]byte(b), &ws); err != nil {
			return nil, err
		}
		all = append(all, ws...)
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("work %d has no word-level sync data", workID)
	}
	return all, nil
}

// splitContent cuts the plaintext right after the boundary phrase. Matching is
// on normalized words so punctuation/spacing differences between the phrase as
// typed and as stored don't defeat it, but the ORIGINAL text is what gets
// stored — the split must not rewrite the transcript.
func splitContent(content, phrase string) (string, string, error) {
	target := norm(phrase)
	if len(target) == 0 {
		return "", "", fmt.Errorf("boundary phrase has no searchable words")
	}

	// Walk the raw text word by word, tracking byte offsets, so the cut lands
	// on a real character position in the original string.
	type tok struct {
		norm     string
		endByte  int
		startByt int
	}
	var toks []tok
	inWord := false
	start := 0
	for i := 0; i <= len(content); i++ {
		isSep := i == len(content) || content[i] == ' ' || content[i] == '\n' ||
			content[i] == '\t' || content[i] == '\r'
		if !isSep && !inWord {
			inWord, start = true, i
		} else if isSep && inWord {
			inWord = false
			n := norm(content[start:i])
			if len(n) > 0 {
				toks = append(toks, tok{norm: strings.Join(n, " "), endByte: i, startByt: start})
			}
		}
	}

	var cut = -1
	matches := 0
	for i := 0; i+len(target) <= len(toks); i++ {
		ok := true
		for j, t := range target {
			if toks[i+j].norm != t {
				ok = false
				break
			}
		}
		if ok {
			matches++
			cut = toks[i+len(target)-1].endByte
		}
	}
	switch {
	case matches == 0:
		return "", "", fmt.Errorf("boundary phrase %q not found in the chapter text", phrase)
	case matches > 1:
		return "", "", fmt.Errorf("boundary phrase %q appears %d times — use a longer phrase", phrase, matches)
	}
	return strings.TrimSpace(content[:cut]), strings.TrimSpace(content[cut:]), nil
}

// applySplit shortens the original chapter and inserts the remainder as the
// next index, shifting any later chapters up. Done in one transaction: a
// half-applied split would leave a duplicate or missing index_num.
func applySplit(sq *sql.DB, workID int64, ch *db.Chapter, head, tail string, endSec, startSec float64, title string) error {
	tx, err := sq.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Shift later chapters DOWNWARD from the end so UNIQUE(book_id, index_num)
	// never collides mid-shift.
	rows, err := tx.Query(`SELECT index_num FROM chapters WHERE book_id = ? AND index_num > ?
	                       ORDER BY index_num DESC`, ch.BookID, ch.Index)
	if err != nil {
		return err
	}
	var later []int
	for rows.Next() {
		var i int
		if err := rows.Scan(&i); err != nil {
			rows.Close()
			return err
		}
		later = append(later, i)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, i := range later {
		if _, err := tx.Exec(`UPDATE chapters SET index_num = index_num + 1
		                      WHERE book_id = ? AND index_num = ?`, ch.BookID, i); err != nil {
			return fmt.Errorf("shift chapter %d: %w", i, err)
		}
	}

	if _, err := tx.Exec(`UPDATE chapters SET content = ?, word_count = ?, end_sec = ?
	                      WHERE id = ?`, head, wordCount(head), endSec, ch.ID); err != nil {
		return fmt.Errorf("shorten original: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO chapters (book_id, index_num, title, src, content, word_count,
	                                            start_sec, end_sec, confidence, content_html)
	                      VALUES (?, ?, ?, '', ?, ?, ?, ?, 0, '')`,
		ch.BookID, ch.Index+1, title, tail, wordCount(tail), startSec, ch.EndSec); err != nil {
		return fmt.Errorf("insert new chapter: %w", err)
	}

	// Local-first sync contract: content_version must move on ANY change to a
	// work's data, or mobile's cheap update-check never re-fetches this book.
	if workID != 0 {
		if _, err := tx.Exec(`UPDATE works SET content_version = ?, updated_at = CURRENT_TIMESTAMP
		                      WHERE id = ?`, time.Now().UTC().Format(time.RFC3339), workID); err != nil {
			return fmt.Errorf("stamp content_version: %w", err)
		}
	}
	return tx.Commit()
}

func trunc(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func tail1(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
