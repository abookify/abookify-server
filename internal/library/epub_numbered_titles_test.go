package library

import (
	"os"
	"strings"
	"testing"
)

// Older Calibre conversions carry no heading tags: every chapter title is an
// ordinary <p>. The Selfish Gene's EPUB is one — "1. Why are people?" through
// "13. The long reach of the gene." — and the tagged-heading split saw nothing,
// so the book fell back to six file-sized lumps and the reader could not follow
// a tapped audio chapter to its text.
func numberedBook(bodies map[int]string) string {
	var b strings.Builder
	b.WriteString(`<h1 class="calibre4"><b>Preface to 1976 edition</b></h1><p>` + strings.Repeat("preface words ", 150) + `</p>`)
	b.WriteString(`<p>Contents</p>`)
	for i := 1; i <= 4; i++ {
		b.WriteString(`<p class="calibre2">` + numTitle(i) + `</p>`)
	}
	for i := 1; i <= 4; i++ {
		b.WriteString(`<p class="calibre2">` + numTitle(i) + `.</p>`)
		body, ok := bodies[i]
		if !ok {
			body = strings.Repeat("chapter "+numTitle(i)+" prose ", 120)
		}
		b.WriteString(`<p class="calibre2">` + body + `</p>`)
	}
	return b.String()
}

func numTitle(i int) string {
	titles := map[int]string{1: "1. Why are people?", 2: "2. The replicators", 3: "3. Immortal coils", 4: "4. The gene machine"}
	return titles[i]
}

func TestSplitHTMLByHeadings_NumberedParagraphTitles(t *testing.T) {
	segs := splitHTMLByHeadings(numberedBook(nil), "", "")
	if segs == nil {
		t.Fatal("numbered title paragraphs were not recognised as chapter boundaries")
	}
	var titles []string
	for _, s := range segs {
		text := strings.TrimSpace(htmlToText(s.html))
		if isHeadingOnly(text, s.title) {
			continue // the contents list collapses to heading-only segments
		}
		titles = append(titles, s.title)
	}
	want := []string{"Preface to 1976 edition", "1. Why are people?", "2. The replicators", "3. Immortal coils", "4. The gene machine"}
	if strings.Join(titles, "|") != strings.Join(want, "|") {
		t.Fatalf("titles = %q, want %q", titles, want)
	}
	// The contents list must not have become the chapter's body: chapter 2's
	// segment starts at its own title paragraph, not at the contents entry.
	for _, s := range segs {
		if s.title == "2. The replicators" && !strings.Contains(s.html, "chapter 2. The replicators prose") {
			t.Fatalf("chapter 2 segment lacks its prose: %q", firstNL(s.html))
		}
	}
}

// A short numbered list inside prose is not a table of contents. Three items
// each followed by a sentence must leave the book alone (nil → spine fallback).
func TestSplitHTMLByHeadings_NumberedListIsNotChapters(t *testing.T) {
	html := `<p>Some prose.</p><p>1. First rule</p><p>Do this.</p><p>2. Second rule</p><p>Do that.</p><p>3. Third rule</p><p>` + strings.Repeat("closing prose ", 300) + `</p>`
	if segs := splitHTMLByHeadings(html, "", ""); segs != nil {
		t.Fatalf("a numbered list split the book into %d segments", len(segs))
	}
}

// A stray numbered paragraph breaking the monotonic chain stands the heuristic
// down entirely rather than producing a half-right split.
func TestSplitHTMLByHeadings_NumberedChainMustIncrease(t *testing.T) {
	bodies := map[int]string{2: strings.Repeat("body ", 120) + `</p><p>1. A list item that reads like a title</p><p>` + strings.Repeat("more body ", 250)}
	if segs := splitHTMLByHeadings(numberedBook(bodies), "", ""); segs != nil {
		t.Fatalf("non-monotonic numbered paragraphs still split the book into %d segments", len(segs))
	}
}

// Tagged chapter headings keep priority: numbered paragraphs are only consulted
// when the book has no <hN> chapter headings at all.
func TestSplitHTMLByHeadings_TaggedHeadingsWin(t *testing.T) {
	html := `<h2>Chapter 1</h2><p>1. A numbered aside</p><p>` + strings.Repeat("x ", 300) + `</p><h2>Chapter 2</h2><p>2. Another aside</p><p>` + strings.Repeat("y ", 300) + `</p><p>3. Third aside</p><p>` + strings.Repeat("z ", 300) + `</p>`
	segs := splitHTMLByHeadings(html, "", "")
	if len(segs) != 2 {
		t.Fatalf("got %d segments, want the 2 tagged chapters", len(segs))
	}
}

// Preface/foreword headings are reading units the narrator reads; they now
// count as boundaries so they are not fused into the leading front matter.
func TestChapterHeadingTextRe_Preface(t *testing.T) {
	for _, s := range []string{"Preface to 1989 edition", "Foreword", "Afterword"} {
		if !chapterHeadingTextRe.MatchString(s) {
			t.Errorf("%q should be a chapter boundary", s)
		}
	}
	for _, s := range []string{"Marley's Ghost", "The Project Gutenberg eBook of Frankenstein"} {
		if chapterHeadingTextRe.MatchString(s) {
			t.Errorf("%q must not be a chapter boundary", s)
		}
	}
}

// Real data: The Selfish Gene (PJ's copy). Skips when the file is not present.
func TestExtractEPUB_SelfishGeneNumberedChapters(t *testing.T) {
	path := os.Getenv("ABOOKIFY_TEST_EPUB_PATH")
	if path == "" {
		path = "../../testdata/library/audiobooks/Richard Dawkins - The Selfish Gene/The Selfish Gene by Richard Dawkins.epub"
	}
	chs, err := ExtractEPUBChapters(path, 1)
	if err != nil {
		t.Skipf("epub not available: %v", err)
	}
	var titles []string
	for _, ch := range chs {
		titles = append(titles, ch.Title)
	}
	t.Logf("%d chapters: %q", len(chs), titles)
	for _, want := range []string{"1. Why are people?", "7. Family planning", "11. Memes: the new replicators", "13. The long reach of the gene", "Preface to 1976 edition", "Preface to 1989 edition"} {
		found := false
		for _, ti := range titles {
			if ti == want {
				found = true
			}
		}
		if !found {
			t.Errorf("missing chapter %q", want)
		}
	}
	for _, ch := range chs {
		if strings.HasPrefix(ch.Title, "1. Why") && (ch.WordCount < 3000 || ch.WordCount > 8000) {
			t.Errorf("chapter 1 has %d words — boundary is wrong", ch.WordCount)
		}
	}
}
