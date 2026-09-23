package library

import (
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strconv"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/pj/abookify/internal/db"
)

// Guarded harness: prints each ebook chapter's aligned audio range for a work
// (what title propagation and the linker reason from). RANGES_DUMP_DB=… RANGES_DUMP_WORK=N
func TestDumpChapterRanges(t *testing.T) {
	if os.Getenv("RANGES_DUMP_WORK") == "" {
		t.Skip("harness")
	}
	wid, _ := strconv.ParseInt(os.Getenv("RANGES_DUMP_WORK"), 10, 64)
	sq, err := sql.Open("sqlite", "file:"+os.Getenv("RANGES_DUMP_DB")+"?mode=ro&_pragma=busy_timeout(15000)")
	if err != nil {
		t.Fatal(err)
	}
	defer sq.Close()
	store := db.NewStoreFromDB(sq)
	work, err := store.GetWork(wid)
	if err != nil || work == nil {
		t.Fatalf("work: %v", err)
	}
	for _, b := range work.TextFiles {
		if b.Format != "epub" {
			continue
		}
		ranges, err := EbookChapterAudioRanges(store, b.ID)
		if err != nil {
			t.Fatal(err)
		}
		chs, _ := store.ListChapters(b.ID)
		sort.Slice(chs, func(i, j int) bool { return chs[i].Index < chs[j].Index })
		for _, c := range chs {
			r, ok := ranges[c.Index]
			fmt.Printf("epub %d ch %2d %-36q range %v ok=%v\n", b.ID, c.Index, c.Title, r, ok)
		}
	}
}

func init() {
	if os.Getenv("RANGES_DUMP_WORK") != "" {
		return
	}
}
