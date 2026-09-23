package library

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// Guarded harness, not a unit test: prints what TODAY's extractor makes of an
// EPUB (index, words, title) so a stored chapter list can be compared with the
// current code's output before deciding between "fix the splitter" and
// "re-derive the book". EPUB_DUMP=<file[,file…]> go test -run TestDumpEpubChapters ./internal/library
func TestDumpEpubChapters(t *testing.T) {
	paths := os.Getenv("EPUB_DUMP")
	if paths == "" {
		t.Skip("EPUB_DUMP not set — harness, not a unit test")
	}
	for _, p := range strings.Split(paths, ",") {
		chapters, err := ExtractEPUBChapters(strings.TrimSpace(p), 0)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		fmt.Printf("== %s: %d chapters\n", p, len(chapters))
		for _, c := range chapters {
			if os.Getenv("EPUB_DUMP_ALL") != "" || c.Index < 6 || c.WordCount < 200 {
				head := c.Content
				if len(head) > 110 {
					head = head[:110]
				}
				fmt.Printf("  %3d %6dw  %q\n        %q\n", c.Index, c.WordCount, strings.ReplaceAll(c.Title, "\n", " / "), strings.ReplaceAll(head, "\n", "⏎"))
			}
		}
	}
}
