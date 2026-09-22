package library

import (
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// Guarded harness, not a unit test: computes the CAS content key the generator
// would use NOW for every chapter of the named TTS editions, so a change to
// preprocessing can be measured against what is on disk (the .mp3.cas
// sidecars) BEFORE it lands: every chapter whose key moves is a chapter that
// regenerates at the next generate. "A one-liner that changes a content key is
// not a one-liner in effect." Run with:
//
//	TTS_KEY_DUMP_DB=data/abookify.db TTS_KEY_DUMP_EDITIONS="111504:bm_fable,106:bf_emma" \
//	TTS_KEY_DUMP_OUT=keys.tsv go test -run TestDumpTTSContentKeys ./internal/library
//
// Editions are <text book id>:<voice> (the generated dir name tts-book-<id>-<voice>
// with the voice's underscore). Pauses default to the generator's defaults
// (1100/500); override with TTS_KEY_DUMP_TITLE_PAUSE_MS / _PARA_PAUSE_MS.
// Output: book_id \t chapter_idx \t key \t title. The DB is opened read-only.
func TestDumpTTSContentKeys(t *testing.T) {
	out := os.Getenv("TTS_KEY_DUMP_OUT")
	if out == "" {
		t.Skip("TTS_KEY_DUMP_OUT not set — harness, not a unit test")
	}
	sq, err := sql.Open("sqlite", "file:"+os.Getenv("TTS_KEY_DUMP_DB")+"?mode=ro&_pragma=busy_timeout(15000)")
	if err != nil {
		t.Fatal(err)
	}
	defer sq.Close()
	titlePause, paraPause := 1100, 500
	if n, _ := strconv.Atoi(os.Getenv("TTS_KEY_DUMP_TITLE_PAUSE_MS")); n > 0 {
		titlePause = n
	}
	if n, _ := strconv.Atoi(os.Getenv("TTS_KEY_DUMP_PARA_PAUSE_MS")); n > 0 {
		paraPause = n
	}
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	total := 0
	for _, ed := range strings.Split(os.Getenv("TTS_KEY_DUMP_EDITIONS"), ",") {
		parts := strings.SplitN(strings.TrimSpace(ed), ":", 2)
		if len(parts) != 2 {
			continue
		}
		bookID, voice := parts[0], parts[1]
		rows, err := sq.Query(`SELECT index_num, title, content FROM chapters WHERE book_id = ? ORDER BY index_num`, bookID)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var idx int
			var title, content string
			if err := rows.Scan(&idx, &title, &content); err != nil {
				t.Fatal(err)
			}
			segments := PreprocessForTTSSegmentsWithPauses(title, content, titlePause, paraPause)
			var segTexts []string
			for _, seg := range segments {
				segTexts = append(segTexts, seg.Text)
			}
			key := TTSContentKey(strings.Join(segTexts, "\n\n"), voice, titlePause, paraPause)
			fmt.Fprintf(f, "%s\t%d\t%s\t%s\n", bookID, idx, key, strings.ReplaceAll(title, "\t", " "))
			total++
		}
		rows.Close()
	}
	t.Logf("dumped %d chapter keys to %s", total, out)
}
