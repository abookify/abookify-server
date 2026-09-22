package library

import (
	"github.com/pj/abookify/internal/db"
	"strings"
	"testing"
)

// htmlToText must not leak HTML-entity or footnote-marker artifacts into the
// plain-text/alignment content — otherwise they show as FALSE diffs in the meld
// (server-web follow-up: 'nbsp', 'four1', 'mizzen mast bc').
func TestHtmlToText_EntitiesAndFootnoteArtifacts(t *testing.T) {
	cases := []struct {
		name string
		in   string
		// substrings that must NOT appear as tokens, and ones that must.
		absent  []string
		present []string
	}{
		{
			name:    "nbsp entity is not a literal token",
			in:      `<p>Chocolat&nbsp;&nbsp;&nbsp;ONE&nbsp;February&nbsp;11</p>`,
			absent:  []string{"nbsp", "&nbsp"},
			present: []string{"Chocolat", "ONE", "February", "11"},
		},
		{
			name:    "superscript footnote marker detached from word",
			in:      `<p>the number four<sup>1</sup> and the mizzen-mast<sup>bc</sup> creaked.</p>`,
			absent:  []string{"four1", "mizzenbc", "mast bc", "mastbc"},
			present: []string{"four", "creaked"},
		},
		{
			name:    "footnote noteref anchor content dropped",
			in:      `<p>He paused<a epub:type="noteref" href="#fn3">3</a> at the door<a href="#footnote7">7</a>.</p>`,
			absent:  []string{"paused3", "door7", "3", "7"},
			present: []string{"paused", "door"},
		},
		{
			name:    "named + numeric entities decode",
			in:      `<p>Tom &amp; Jerry said &#8220;hi&#8221; &mdash; nice.</p>`,
			absent:  []string{"amp", "8220", "8221", "mdash"},
			present: []string{"Tom", "Jerry", "nice"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := htmlToText(c.in)
			toks := Tokenize(out) // the alignment tokenizer
			tokset := map[string]bool{}
			for _, tk := range toks {
				tokset[tk] = true
			}
			for _, a := range c.absent {
				// absent as a whole token (after normalization) …
				if tokset[strings.ToLower(strings.Trim(a, "& "))] {
					t.Errorf("got false token %q in %q (text=%q)", a, toks, out)
				}
				// … and absent as a substring of the raw text where it'd be glued.
				if strings.Contains(strings.ToLower(out), strings.ToLower(a)) && !strings.Contains(a, " ") {
					t.Errorf("artifact %q survived in text %q", a, out)
				}
			}
			for _, p := range c.present {
				if !tokset[strings.ToLower(p)] {
					t.Errorf("expected token %q missing from %q (text=%q)", p, toks, out)
				}
			}
		})
	}
}

func TestTrimGutenbergBoilerplate(t *testing.T) {
	// Shape taken from a real PG epub (#75011): sentinels sit in a <span>
	// inside pg-boilerplate divs, licence text runs to the end of the file.
	const doc = `<div id="pg-header"><p>Title: All Quiet</p><p>Release date: 2025</p>
<div id="pg-start-separator">
<span>*** START OF THE PROJECT GUTENBERG EBOOK ALL QUIET ON THE WESTERN FRONT ***</span>
</div></div>
<h2>CHAPTER I</h2><p>We are at rest five miles behind the front.</p>
<h2>CHAPTER XII</h2><p>It is autumn.</p>
<div class="pg-boilerplate pgheader footer" id="pg-footer">
<div id="pg-end-separator">
<span>*** END OF THE PROJECT GUTENBERG EBOOK ALL QUIET ON THE WESTERN FRONT ***</span>
</div>
<p>Updated editions will replace the previous one.</p>
<p>Project Gutenberg is a registered trademark, and may not be used if you charge for an eBook.</p></div>`

	got := trimGutenbergBoilerplate(doc)

	for _, leaked := range []string{"Release date", "registered trademark", "Updated editions"} {
		if strings.Contains(got, leaked) {
			t.Errorf("boilerplate %q survived the trim:\n%s", leaked, got)
		}
	}
	for _, kept := range []string{"CHAPTER I", "five miles behind the front", "CHAPTER XII", "It is autumn"} {
		if !strings.Contains(got, kept) {
			t.Errorf("book text %q was trimmed away:\n%s", kept, got)
		}
	}
}

func TestTrimGutenbergBoilerplate_NonGutenbergUntouched(t *testing.T) {
	// A publisher epub has no sentinels and must come back byte-identical —
	// the trim must never guess at where a non-PG book starts or ends.
	const doc = `<h1>Chapter One</h1><p>It was a bright cold day in April.</p>`
	if got := trimGutenbergBoilerplate(doc); got != doc {
		t.Errorf("non-PG document was modified:\ngot  %q\nwant %q", got, doc)
	}
}

func TestTrimGutenbergBoilerplate_ThisVariantAndOnlyFooter(t *testing.T) {
	// Older PG files say "THIS PROJECT GUTENBERG EBOOK"; and a spine file may
	// contain only the footer sentinel (extractPerSpineFile trims per file).
	const doc = `<p>The last line of the book.</p>
<span>*** END OF THIS PROJECT GUTENBERG EBOOK ALICE ***</span>
<p>Section 1. General Terms of Use.</p>`
	got := trimGutenbergBoilerplate(doc)
	if strings.Contains(got, "General Terms of Use") {
		t.Errorf("footer licence survived:\n%s", got)
	}
	if !strings.Contains(got, "The last line of the book.") {
		t.Errorf("book text was trimmed away:\n%s", got)
	}
}

func TestTrimGutenbergBoilerplate_PreFenceSignOff(t *testing.T) {
	// Pre-2020 PG files put a bare sign-off line BEFORE the fenced end marker,
	// so cutting only at the fence leaves it as the book's last words.
	const doc = `<p>They have a world to win. WORKING MEN OF ALL COUNTRIES, UNITE!</p>
<p>End of the Project Gutenberg EBook of The Communist Manifesto, by Karl Marx</p>
<span>*** END OF THIS PROJECT GUTENBERG EBOOK THE COMMUNIST MANIFESTO ***</span>
<p>Section 1. General Terms of Use.</p>`
	got := trimGutenbergBoilerplate(doc)
	if strings.Contains(got, "End of the Project Gutenberg") {
		t.Errorf("bare sign-off survived:\n%s", got)
	}
	if !strings.Contains(got, "WORKING MEN OF ALL COUNTRIES, UNITE!") {
		t.Errorf("book text was trimmed away:\n%s", got)
	}
}

// A closing </p> must yield a paragraph break (blank line), distinct from
// <br>/hard-wraps — the one-character defect that flattened all 53 library
// epubs (2026-08-10).
func TestExtractPreservesParagraphBreaks(t *testing.T) {
	html := `<html><body><p>Marley was dead:
to begin with.</p><p>There is no doubt<br/>whatever about that.</p></body></html>`
	text := htmlToText(html)
	if !strings.Contains(text, "\n\n") {
		t.Fatalf("no paragraph break survived extraction: %q", text)
	}
	paras := strings.Split(text, "\n\n")
	if len(paras) != 2 {
		t.Fatalf("want 2 paragraphs, got %d: %q", len(paras), text)
	}
	if strings.Contains(paras[0], "\n") {
		t.Errorf("intra-paragraph wrap should be healed to a space: %q", paras[0])
	}
}

// Some publisher EPUBs use <div> as the paragraph element (Vintage's Gulag
// Archipelago, Recorded Books' Crime and Punishment). The whitelist dropped
// <div> silently, so content_html became one run of <span>s and the reader
// rendered the whole chapter as one block (board 11). <div> is now a <p>, and
// the wrapper nesting that leaves is folded to one level.
func TestSanitizeHTMLTreatsDivAsParagraph(t *testing.T) {
	raw := `<body><div class="body"><div class="para">Once <span>we</span> have taken up the word.</div>
<div class="para">A writer is no detached judge.</div><div class="empty"> </div></div></body>`
	got := sanitizeHTML(raw)
	if n := strings.Count(got, "<p>"); n != 2 {
		t.Fatalf("want 2 paragraphs, got %d in %q", n, got)
	}
	if strings.Contains(got, "<p><p>") || strings.Contains(got, "</p></p>") || strings.Contains(got, "<p></p>") {
		t.Errorf("wrapper nesting not folded: %q", got)
	}
	if !strings.Contains(got, "<p>Once <span>we</span> have taken up the word.</p>") {
		t.Errorf("paragraph text/inline markup lost: %q", got)
	}
	// Real <p> documents are untouched by the rewrite.
	if got := sanitizeHTML(`<p>One.</p><p>Two.</p>`); got != `<p>One.</p><p>Two.</p>` {
		t.Errorf("plain <p> document changed: %q", got)
	}
}

// A stranger's first press of play landed on a colophon (2026-09-22): every
// Gutenberg EPUB leads with a title page, a contents list and sometimes a
// dedication or note, each of which had become a chapter of its own. The
// leading stubs fold: colophon and contents dropped, the note carried into
// chapter I, and chapter I titled by its own heading rather than the book's
// running title stamped above it.
func TestFoldFrontMatterDraculaShape(t *testing.T) {
	note := strings.Repeat("How these papers have been placed in sequence will be made manifest in the reading of them. ", 4)
	body := strings.Repeat("Left Munich at 8:35 P. M., on 1st May, arriving at Vienna early next morning. ", 40)
	in := []db.Chapter{
		{Title: "D R A C U L A", Content: "D R A C U L A\n\nby\n\nBram Stoker\n\nNEW YORK\n\nGROSSET & DUNLAP\n\nPublishers\n\nCopyright, 1897, in the United States of America"},
		{Title: "Contents", Content: "TO\n\nMY DEAR FRIEND\n\nHOMMY-BEG\n\nContents\n\nCHAPTER I. Jonathan Harker’s Journal\n\nCHAPTER II. Jonathan Harker’s Journal"},
		{Title: "Chapter 3", Content: note, ContentHTML: "<p>" + note + "</p>"},
		{Title: "CHAPTER I\n\nJONATHAN HARKER’S JOURNAL", Content: "D R A C U L A\n\nCHAPTER I\n\nJONATHAN HARKER’S JOURNAL\n\n" + body, ContentHTML: "<h2>D R A C U L A</h2><h2>CHAPTER I</h2><p>" + body + "</p>"},
		{Title: "CHAPTER II\n\nJONATHAN HARKER’S JOURNAL—continued", Content: "CHAPTER II\n\n" + body},
	}
	for i := range in {
		in[i].WordCount = len(strings.Fields(in[i].Content))
	}
	got := foldFrontMatter(in, "Dracula")
	if len(got) != 2 {
		t.Fatalf("want 2 chapters (I, II), got %d: %v", len(got), titlesOf(got))
	}
	if !strings.HasPrefix(got[0].Content, "How these papers") {
		t.Errorf("the note was not folded into chapter I: %q", got[0].Content[:60])
	}
	if strings.Contains(got[0].Content, "GROSSET") || strings.Contains(got[0].Content, "HOMMY-BEG") {
		t.Errorf("colophon or contents leaked into chapter I")
	}
	if strings.Contains(got[0].Content, "\nD R A C U L A\n") || strings.HasPrefix(got[0].ContentHTML, "<h2>D R A C U L A") {
		t.Errorf("running book title not stripped from chapter I: %q / %q", got[0].Content[:80], got[0].ContentHTML[:40])
	}
	if !strings.HasPrefix(got[0].ContentHTML, "<p>How these papers") {
		t.Errorf("html prefix wrong: %q", got[0].ContentHTML[:60])
	}
	if got[1].Title != in[4].Title {
		t.Errorf("chapter II changed: %q", got[1].Title)
	}
}

// The Selfish Gene's lead section is a page of review quotes: large, no
// heading, titled "Front matter" by the splitter. That is not a stub and
// stays a chapter; a book that is all short chapters stays exactly as it was.
func TestFoldFrontMatterLeavesRealSectionsAlone(t *testing.T) {
	blurbs := strings.Repeat("A brilliant book, said a reviewer. ", 60)
	body := strings.Repeat("Intelligent life on a planet comes of age. ", 60)
	in := []db.Chapter{
		{Title: "Front matter", Content: blurbs, WordCount: len(strings.Fields(blurbs))},
		{Title: "1. Why are people?", Content: body, WordCount: len(strings.Fields(body))},
	}
	if got := foldFrontMatter(in, "The Selfish Gene"); len(got) != 2 || got[0].Title != "Front matter" {
		t.Errorf("large lead section must survive: %v", titlesOf(got))
	}
	short := []db.Chapter{
		{Title: "I", Content: "One short poem.", WordCount: 3},
		{Title: "II", Content: "Another short poem.", WordCount: 3},
	}
	if got := foldFrontMatter(short, "Poems"); len(got) != 2 {
		t.Errorf("all-short book must be untouched: %v", titlesOf(got))
	}
}

func TestExtractChapterHeadingPrefersChapterLine(t *testing.T) {
	h := `<div class="chapter"><h2>D R A C U L A</h2><hr/></div><div class="chapter"><h2><a id="chap01"/>CHAPTER I<br/><br/><small>JONATHAN HARKER’S JOURNAL</small></h2><p>3 May.</p>`
	if got := extractChapterHeading(h); !strings.HasPrefix(got, "CHAPTER I") {
		t.Errorf("want the CHAPTER I heading, got %q", got)
	}
	if got := extractChapterHeading(`<h1>A Preface Note</h1><p>x</p>`); got != "A Preface Note" {
		t.Errorf("fallback to first heading broken: %q", got)
	}
}

func titlesOf(cs []db.Chapter) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = strings.ReplaceAll(c.Title, "\n", " / ")
	}
	return out
}

// Carol's leading unit is a title page that runs into Dickens's own preface.
// The preface is his words and stays — as its own chapter, not folded into
// Stave One (whose text, and therefore whose content key and narration, must
// not move for this).
func TestFoldFrontMatterKeepsPrefaceAsChapter(t *testing.T) {
	lead := "Cover of 1843 First Edition\n\nTitle Page of 1843 First Edition\n\nA CHRISTMAS CAROL\n\nIN PROSE\n\nBEING\n\nA Ghost Story of Christmas\n\nBY\n\nCHARLES DICKENS\n\nWITH ILLUSTRATIONS BY JOHN LEECH\n\nPREFACE\n\n" +
		"I HAVE endeavoured in this Ghostly little book, to raise the Ghost of an Idea, which shall not put my readers out of humour with themselves, with each other, with the season, or with me. May it haunt their houses pleasantly, and no one wish to lay it. Their faithful Friend and Servant, C. D. December, 1843."
	stave := strings.Repeat("Marley was dead: to begin with. There is no doubt whatever about that. ", 30)
	in := []db.Chapter{
		{Title: "A CHRISTMAS CAROL", Content: lead, ContentHTML: "<p>Cover of 1843 First Edition</p><h1>A CHRISTMAS CAROL</h1><h2>PREFACE</h2><p>I HAVE endeavoured…</p>"},
		{Title: "STAVE ONE.", Content: "STAVE ONE.\n\nMARLEY’S GHOST.\n\n" + stave},
	}
	for i := range in {
		in[i].WordCount = len(strings.Fields(in[i].Content))
	}
	got := foldFrontMatter(in, "A Christmas Carol in Prose; Being a Ghost Story of Christmas")
	if len(got) != 2 || got[0].Title != "Preface" || got[1].Title != "STAVE ONE." {
		t.Fatalf("want [Preface, STAVE ONE.], got %v", titlesOf(got))
	}
	if !strings.HasPrefix(got[0].Content, "I HAVE endeavoured") || strings.Contains(got[0].Content, "JOHN LEECH") {
		t.Errorf("preface content wrong: %q", got[0].Content[:60])
	}
	if !strings.HasPrefix(got[0].ContentHTML, "<h2>PREFACE</h2>") {
		t.Errorf("preface html should start at the marker: %q", got[0].ContentHTML[:40])
	}
	if got[1].Content != in[1].Content {
		t.Errorf("Stave One's text must not move")
	}
}

// Oz leads with a title page, a contents list whose entries include the word
// "Introduction", and then Baum's real Introduction. The list is dropped, the
// Introduction is kept as its own chapter, and neither becomes a preface by
// mistake.
func TestFoldFrontMatterContentsListIsNotAPreface(t *testing.T) {
	contents := "The Wonderful Wizard of Oz\n\nby L. Frank Baum\n\nContents\n\nIntroduction\n\nChapter I. The Cyclone\n\nChapter II. The Council with the Munchkins\n\nChapter III. How Dorothy Saved the Scarecrow\n\nChapter IV. The Road Through the Forest\n\nChapter V. The Rescue of the Tin Woodman\n\nChapter VI. The Cowardly Lion\n\nChapter VII. The Journey to the Great Oz\n\nChapter VIII. The Deadly Poppy Field\n\nChapter IX. The Queen of the Field Mice"
	intro := "Introduction\n\n" + strings.Repeat("Folklore, legends, myths and fairy tales have followed childhood through the ages. ", 12)
	body := strings.Repeat("Dorothy lived in the midst of the great Kansas prairies, with Uncle Henry. ", 40)
	in := []db.Chapter{
		{Title: "The Wonderful Wizard of Oz", Content: contents},
		{Title: "Introduction", Content: intro},
		{Title: "Chapter I\n\nThe Cyclone", Content: "Chapter I\n\nThe Cyclone\n\n" + body},
	}
	for i := range in {
		in[i].WordCount = len(strings.Fields(in[i].Content))
	}
	got := foldFrontMatter(in, "The Wonderful Wizard of Oz")
	if len(got) != 2 || got[0].Title != "Introduction" || !strings.HasPrefix(got[1].Title, "Chapter I") {
		t.Fatalf("want [Introduction, Chapter I], got %v", titlesOf(got))
	}
	if strings.Contains(got[0].Content, "Cyclone") || strings.Contains(got[1].Content, "Munchkins\n") {
		t.Errorf("contents list leaked: %q / %q", got[0].Content[:50], got[1].Content[:50])
	}
	if !looksLikeContentsList(contents) || looksLikeContentsList(intro) {
		t.Errorf("contents-list detector wrong")
	}
}
