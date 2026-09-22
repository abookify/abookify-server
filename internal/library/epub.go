package library

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	gohtml "html"
	"io"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/pj/abookify/internal/db"
)

// spine item from the OPF
type spineItemref struct {
	IDRef  string `xml:"idref,attr"`
	Linear string `xml:"linear,attr"`
}

type manifestItem struct {
	ID        string `xml:"id,attr"`
	Href      string `xml:"href,attr"`
	MediaType string `xml:"media-type,attr"`
}

type opfPackage struct {
	Metadata struct {
		Title   []string `xml:"title"`
		Creator []string `xml:"creator"`
	} `xml:"metadata"`
	Manifest struct {
		Items []manifestItem `xml:"item"`
	} `xml:"manifest"`
	Spine struct {
		Itemrefs []spineItemref `xml:"itemref"`
	} `xml:"spine"`
}

// navPoint from NCX table of contents
type navPoint struct {
	Label struct {
		Text string `xml:"text"`
	} `xml:"navLabel>text"`
	Content struct {
		Src string `xml:"src,attr"`
	} `xml:"content"`
	Children []navPoint `xml:"navPoint"`
}

type ncxDoc struct {
	NavMap struct {
		NavPoints []navPoint `xml:"navPoint"`
	} `xml:"navMap"`
}

var htmlTagRe = regexp.MustCompile(`<[^>]*>`)

// headRe matches the whole <head> block, whose <title> text would otherwise
// survive tag-stripping and duplicate the chapter heading.
var headRe = regexp.MustCompile(`(?is)<head[^>]*>.*?</head>`)
var whitespaceRe = regexp.MustCompile(`\s+`)

// ExtractEPUBChapters parses an EPUB and returns its chapters with text content.
func ExtractEPUBChapters(epubPath string, bookID int64) ([]db.Chapter, error) {
	r, err := zip.OpenReader(epubPath)
	if err != nil {
		return nil, fmt.Errorf("open epub: %w", err)
	}
	defer r.Close()

	// Parse container.xml to find the OPF
	opfPath, err := findOPFPath(&r.Reader)
	if err != nil {
		return nil, err
	}
	opfDir := path.Dir(opfPath)

	// Parse OPF
	opfData, err := readZipFile(&r.Reader, opfPath)
	if err != nil {
		return nil, fmt.Errorf("read OPF: %w", err)
	}

	var pkg opfPackage
	if err := xml.Unmarshal(opfData, &pkg); err != nil {
		return nil, fmt.Errorf("parse OPF: %w", err)
	}

	// Build manifest lookup: id -> item
	manifest := map[string]manifestItem{}
	for _, item := range pkg.Manifest.Items {
		manifest[item.ID] = item
	}

	// Try to load NCX for chapter titles
	tocTitles := map[string]string{} // src (without fragment) -> title
	for _, item := range pkg.Manifest.Items {
		if item.MediaType == "application/x-dtbncx+xml" {
			ncxPath := resolvePath(opfDir, item.Href)
			ncxData, err := readZipFile(&r.Reader, ncxPath)
			if err == nil {
				var ncx ncxDoc
				if xml.Unmarshal(ncxData, &ncx) == nil {
					flattenNavPoints(ncx.NavMap.NavPoints, tocTitles, path.Dir(ncxPath))
				}
			}
			break
		}
	}

	// Concatenate content spine items in reading order, then split on chapter
	// headings. Some Project Gutenberg EPUBs split files MID-chapter and pack
	// several chapters per file, so a chapter can span file boundaries — one
	// chapter per spine file (the old behavior) buried and mislabeled them.
	// Concatenating first reconstructs whole chapters regardless of where the
	// publisher cut the files.
	var book strings.Builder
	firstHref := ""
	for _, itemref := range pkg.Spine.Itemrefs {
		if itemref.Linear == "no" {
			continue
		}
		item, ok := manifest[itemref.IDRef]
		if !ok {
			continue
		}
		// Only process XHTML/HTML content
		if !strings.Contains(item.MediaType, "html") && !strings.Contains(item.MediaType, "xml") {
			continue
		}
		content, err := readZipFile(&r.Reader, resolvePath(opfDir, item.Href))
		if err != nil {
			continue
		}
		if firstHref == "" {
			firstHref = item.Href
		}
		book.WriteString(string(content))
		book.WriteString("\n")
	}

	bookHTML := trimGutenbergBoilerplate(book.String())

	var chapters []db.Chapter
	chapterIdx := 0

	// Split on chapter headings. nil => no chapter headings detected, so fall
	// back to the original one-chapter-per-spine-file extraction (correct for
	// EPUBs that put one chapter per file or use non-standard chapter titles).
	segments := splitHTMLByHeadings(bookHTML)
	if segments == nil {
		perFile, err := extractPerSpineFile(&r.Reader, pkg, manifest, opfDir, tocTitles, bookID)
		if err != nil {
			return nil, err
		}
		return cleanExtractedChapters(perFile, epubTitle(pkg)), nil
	}

	for _, seg := range segments {
		text := strings.TrimSpace(htmlToText(seg.html))
		if len(text) < 20 {
			// Skip near-empty front matter / stray heading fragments
			continue
		}

		if isHeadingOnly(text, seg.title) {
			// A document containing only its own heading is a Calibre split
			// artefact, not a chapter. Left in, it becomes an embedded chunk and
			// gets cited as though it were book text.
			continue
		}

		title := seg.title
		if title == "" {
			title = tocTitles[stripFragment(firstHref)]
		}
		if title == "" && seg.lead {
			// Title page, blurbs, dedication — whatever precedes the first
			// chapter boundary without a heading of its own. Calling it
			// "Chapter 1" put a page of review quotes ahead of the real
			// chapter 1 in The Selfish Gene's TOC.
			title = "Front matter"
		}
		if title == "" {
			title = fmt.Sprintf("Chapter %d", chapterIdx+1)
		}

		chapters = append(chapters, db.Chapter{
			BookID:      bookID,
			Index:       chapterIdx,
			Title:       title,
			Src:         firstHref,
			Content:     text,
			ContentHTML: sanitizeHTML(seg.html),
			WordCount:   len(strings.Fields(text)),
		})
		chapterIdx++
	}

	// The heading split can be COARSER than the publisher's own file split.
	// chapterHeadingTextRe deliberately accepts "Part"/"Book"/"Volume" — needed
	// where those ARE the chapters — but in a book divided into parts that each
	// contain many chapters, and whose chapter titles carry no number ("The Old
	// Sea-dog at the Admiral Benbow"), only the part headings match. Treasure
	// Island then collapses from 34 chapters to 6 twelve-thousand-word slabs,
	// and War of the Worlds from 27 to 2.
	//
	// The publisher's spine split is the other opinion about where chapters
	// begin, so take whichever is finer rather than letting a heading split win
	// merely because it exists. Both are cheap to compute on an epub.
	if perFile, err := extractPerSpineFile(&r.Reader, pkg, manifest, opfDir, tocTitles, bookID); err == nil &&
		len(perFile) > len(chapters) {
		return cleanExtractedChapters(perFile, epubTitle(pkg)), nil
	}

	return cleanExtractedChapters(chapters, epubTitle(pkg)), nil
}

// Project Gutenberg wraps every ebook in a licence header and footer, fenced by
// these sentinels. The fence text has been stable across PG's epub generations
// (the surrounding markup has not), so match on it rather than on the
// `pg-boilerplate` classes that only modern PG files carry.
var (
	pgStartRe = regexp.MustCompile(`(?is)\*\*\*\s*START OF (?:THE|THIS) PROJECT GUTENBERG EBOOK.*?\*\*\*`)
	pgEndRe   = regexp.MustCompile(`(?is)\*\*\*\s*END OF (?:THE|THIS) PROJECT GUTENBERG EBOOK.*?\*\*\*`)
	// Pre-2020 PG files precede the fenced end marker with a bare sign-off
	// line ("End of the Project Gutenberg EBook of Siddhartha, by Herman
	// Hesse"). Cutting only at the fence leaves that line as the last words of
	// the book.
	pgEndPlainRe = regexp.MustCompile(`(?i)\bEnd of (?:the )?Project Gutenberg(?:'?s)? EBook\b`)
)

// trimGutenbergBoilerplate drops everything outside the PG sentinels.
//
// Left in, the ~2,900-word licence fuses onto the FINAL chapter (there is no
// heading to split it off) and the header becomes a phantom leading chapter.
// That is wrong twice over: the reader shows the licence as the end of the
// book, and alignment counts those words as ebook content the narrator skipped,
// depressing the ebook→audio coverage of every PG-sourced work.
//
// Cutting at the sentinel can leave orphaned closing tags; harmless, since both
// callers immediately run the result through htmlToText / sanitizeHTML. A file
// with no sentinels (any non-PG epub) is returned untouched.
func trimGutenbergBoilerplate(html string) string {
	if loc := pgStartRe.FindStringIndex(html); loc != nil {
		html = html[loc[1]:]
	}
	// Re-scan AFTER the leading cut — the earlier trim shifts every offset.
	if loc := pgEndRe.FindStringIndex(html); loc != nil {
		html = html[:loc[0]]
	}
	// The bare sign-off sits BEFORE the fence, so it survives the cut above.
	if loc := pgEndPlainRe.FindStringIndex(html); loc != nil {
		html = html[:loc[0]]
	}
	return html
}

// Two Project Gutenberg front-matter blocks slip past trimGutenbergBoilerplate
// (they sit INSIDE the content, not between the START/END sentinels) AND past the
// running-head pass (they are multi-line blocks, not one short repeated line):
//  1. the "editions of this ebook" listing — an intro line, a "click the
//     filenumbers" line, then N "<number> (edition description)" rows; and
//  2. a "Project Gutenberg Editor's Note:" label + its one-sentence note.
//
// Both carry PG-unique phrasing that never occurs in an author's prose, so they
// match precisely with no risk to real text — a chapter that merely mentions
// "Gutenberg" (or names the printer), or a prose line that happens to start with
// a number in parentheses, is untouched (see the tests).
var (
	reEditionsIntro = regexp.MustCompile(`(?i)^There are several editions of this ebook in the Project Gutenberg`)
	reClickFilenums = regexp.MustCompile(`(?i)^Click on any of the file ?numbers`)
	reFileEntry     = regexp.MustCompile(`^\d{2,6}\s*\(`)
	reEditorsNote   = regexp.MustCompile(`(?i)Project Gutenberg('?s)? Editor'?s Note`)
)

// stripGutenbergApparatus removes the two PG front-matter blocks above from a
// chapter's plaintext, line by line — keeping everything that is not apparatus,
// so the book's own title page, table of contents and prose all survive. It is
// deliberately narrow (matches only PG-unique phrasing) rather than "drop any
// line mentioning Gutenberg", which would eat legitimate text.
func stripGutenbergApparatus(text string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	inEditions := false
	for i := 0; i < len(lines); i++ {
		norm := strings.TrimSpace(lines[i])
		switch {
		case reEditionsIntro.MatchString(norm):
			inEditions = true // drop the intro and enter the listing
			continue
		case inEditions && (norm == "" || reClickFilenums.MatchString(norm) || reFileEntry.MatchString(norm)):
			continue // still inside the listing
		case inEditions:
			inEditions = false // first non-apparatus line: real content resumes — keep it
		}
		if reEditorsNote.MatchString(norm) {
			// A bare "…Editor's Note:" label is followed by the note sentence on
			// the next line; drop both. An inline note is one line; drop just it.
			if strings.HasSuffix(norm, ":") && i+1 < len(lines) {
				i++
			}
			continue
		}
		out = append(out, lines[i])
	}
	return strings.Join(out, "\n")
}

// stripGutenbergApparatusHTML mirrors the plaintext strip for the reader's rich
// HTML: it removes the block element whose visible text IS the "editions of this
// ebook" intro or a "Project Gutenberg Editor's Note", so the reader's first
// screen for a PG classic isn't apparatus. Same narrowness as
// stripHeaderBlocksFromHTML — it only drops a block whose entire tag-stripped
// text matches PG-unique phrasing, so it can never eat prose. (The plaintext
// strip is the correctness fix for search/chunks/alignment; this only tidies the
// display. It deliberately does NOT touch bare edition-number entries that sit
// outside a block, since a "<year> (published …)" text-node regex could match
// real prose — the safe, provable win is the branded intro/note block.)
func stripGutenbergApparatusHTML(html string) string {
	if html == "" {
		return html
	}
	return htmlApparatusTagRe.ReplaceAllStringFunc(html, func(block string) string {
		inner := strings.TrimSpace(htmlTagRe.ReplaceAllString(block, ""))
		if reEditionsIntro.MatchString(inner) || reEditorsNote.MatchString(inner) {
			return ""
		}
		return block
	})
}

// htmlApparatusTagRe matches a single block OR inline element — the PG apparatus
// label lands in a bare <b>/<span> as often as a <p> ("<b>Project Gutenberg
// Editor's Note:</b>"). Non-greedy + the text-match guard in the caller keeps it
// to elements whose whole text IS the apparatus phrase, so it can't eat prose.
var htmlApparatusTagRe = regexp.MustCompile(`(?is)<(p|h[1-6]|div|b|i|em|strong|span)\b[^>]*>.*?</(?:p|h[1-6]|div|b|i|em|strong|span)>`)

// cleanExtractedChapters is the single funnel every extraction path returns
// through: it strips the PG apparatus blocks (recomputing word counts, dropping
// any chapter emptied by the strip, re-indexing), then runs the running-header
// pass. Splitting it out keeps the apparatus strip unconditional — the
// running-head pass skips books with too few chapters, but front matter needs
// cleaning regardless of length.
func cleanExtractedChapters(chapters []db.Chapter, bookTitle string) []db.Chapter {
	cleaned := make([]db.Chapter, 0, len(chapters))
	for _, ch := range chapters {
		ch.Content = strings.TrimSpace(stripGutenbergApparatus(ch.Content))
		if ch.Content == "" {
			continue
		}
		ch.ContentHTML = stripGutenbergApparatusHTML(ch.ContentHTML)
		ch.WordCount = len(strings.Fields(ch.Content))
		cleaned = append(cleaned, ch)
	}
	cleaned = foldFrontMatter(cleaned, bookTitle)
	for i := range cleaned {
		cleaned[i].Index = i
	}
	return stripRunningHeaders(cleaned)
}

// Front matter (2026-09-22). A stranger's first press of play on our own
// Dracula sample landed on twenty-six seconds of karaoke over "NEW YORK
// GROSSET & DUNLAP, Copyright 1897", then a 28-second stub titled "Chapter 3"
// (the "How these papers have been placed in sequence" note, which had no
// heading of its own), then a chapter I titled with the book's own name. Every
// Gutenberg EPUB in the library carries the same shape: a title page, a
// contents list, sometimes a dedication or a note, all BEFORE the first real
// chapter, each emitted as a chapter of its own and each narrated by TTS.
//
// Policy, applied to the LEADING chapters only (everything before the first
// substantive one), the same fold-forward rule the transcript splitter uses
// for narrator header stubs:
//   - a colophon (title page / copyright page: tiny, titled with the book's
//     name or carrying publisher/copyright wording)  → DROPPED
//   - a boilerplate-titled unit (Contents, licence)   → DROPPED
//   - any other tiny unit (dedication, epigraph, a note) → FOLDED FORWARD as
//     the opening paragraphs of the first real chapter — its words are book
//     text and stay readable and narrated; it just isn't a chapter
//
// A large lead section ("Front matter" blurbs in The Selfish Gene) is left
// alone: this is about stubs, not about deciding what a preface is.
//
// Separately, a chapter whose FIRST LINE is the book's own name (the PG
// pattern <h2>D R A C U L A</h2> immediately before <h2>CHAPTER I</h2>) loses
// that line — it is the running title, not the chapter's text.
const frontMatterMaxWords = 150

var colophonWordsRe = regexp.MustCompile(`(?i)\b(copyright|all rights reserved|published by|publishers?|printed in|first published|isbn)\b`)

func normalizeTitleKey(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// isBookTitle reports whether a heading/line IS the book's title — spaced
// caps ("D R A C U L A"), punctuation and case ignored; a short-form title
// that the full title starts with ("Frankenstein;" vs "Frankenstein; or, the
// Modern Prometheus") counts too.
func isBookTitle(s, bookTitle string) bool {
	k, bk := normalizeTitleKey(s), normalizeTitleKey(bookTitle)
	if k == "" || bk == "" {
		return false
	}
	return k == bk || (len(k) >= 6 && strings.HasPrefix(bk, k))
}

// colophonTitledMaxWords: a LEADING unit titled with the book's own name is a
// title page even when it carries a contents list (Peter Pan: 156 words). A
// real first chapter that happens to share the book's name runs far longer.
const colophonTitledMaxWords = 400

func isColophonChapter(ch db.Chapter, bookTitle string) bool {
	if isBookTitle(ch.Title, bookTitle) && ch.WordCount < colophonTitledMaxWords {
		return true
	}
	return ch.WordCount < frontMatterMaxWords && colophonWordsRe.MatchString(ch.Content)
}

func foldFrontMatter(chapters []db.Chapter, bookTitle string) []db.Chapter {
	// Running title lines and book-named chapters first, while each chapter's
	// first line is still its own (the fold below prepends text to chapter I).
	chapters = stripLeadingBookTitleLines(chapters, bookTitle)
	// Where does the book proper start? The first chapter that is neither
	// tiny nor boilerplate-titled. Everything before it is front matter.
	first := -1
	for i, ch := range chapters {
		if ch.WordCount >= frontMatterMaxWords && !isBoilerplateTitle(ch.Title) && !isColophonChapter(ch, bookTitle) {
			first = i
			break
		}
	}
	if first <= 0 {
		// Nothing leads the first real chapter (or no chapter is substantive —
		// a tiny book stays exactly as extracted).
		return chapters
	}
	var kept []db.Chapter // prefatory units that stay chapters of their own
	var prefixText, prefixHTML []string
	for _, ch := range chapters[:first] {
		// A title page that runs straight into the author's own preface
		// (Dickens: "A CHRISTMAS CAROL … BY CHARLES DICKENS / PREFACE / I HAVE
		// endeavoured…") keeps the preface as a chapter named for it and
		// drops the page above the marker.
		if pre, ok := prefaceFromLead(ch); ok {
			kept = append(kept, pre)
			continue
		}
		switch {
		case isColophonChapter(ch, bookTitle):
			continue
		case isBoilerplateTitle(ch.Title), looksLikeContentsList(ch.Content):
			continue
		default:
			prefixText = append(prefixText, ch.Content)
			if ch.ContentHTML != "" {
				prefixHTML = append(prefixHTML, ch.ContentHTML)
			}
		}
	}
	out := make([]db.Chapter, 0, len(chapters)-first+len(kept))
	out = append(out, kept...)
	body := chapters[first]
	if len(prefixText) > 0 {
		body.Content = strings.Join(prefixText, "\n\n") + "\n\n" + body.Content
		if body.ContentHTML != "" || len(prefixHTML) > 0 {
			body.ContentHTML = strings.Join(prefixHTML, "") + body.ContentHTML
		}
		body.WordCount = len(strings.Fields(body.Content))
	}
	out = append(out, body)
	out = append(out, chapters[first+1:]...)
	return out
}

// looksLikeContentsList: most non-blank lines name chapters ("Chapter I. The
// Cyclone", "II. THE FALLING STAR.") — a table of contents whatever its
// heading says, never text to fold into a chapter.
func looksLikeContentsList(content string) bool {
	total, chapterish := 0, 0
	for _, l := range strings.Split(content, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		total++
		if chapterHeadingTextRe.MatchString(l) && len(strings.Fields(l)) <= 12 {
			chapterish++
		}
	}
	return chapterish >= 3 && chapterish*2 >= total
}

// apparatusMarkerRe: a line that opens the front-matter apparatus that may
// FOLLOW a preface in the same unit — the preface ends there.
var apparatusMarkerRe = regexp.MustCompile(`(?im)^\s*(contents|table of contents|illustrations|list of illustrations)\.?\s*$`)

// prefaceMarkerRe: a line that opens the author's own prefatory text.
var prefaceMarkerRe = regexp.MustCompile(`(?im)^\s*(preface|foreword|introduction|prologue|author'?s note|a note on the text)\.?\s*$`)

// prefaceFromLead splits a leading unit at its preface marker: the marker
// line becomes the chapter's title and everything from it on its content;
// what precedes (title page, illustration captions) is dropped. Only fires
// when real text follows the marker.
func prefaceFromLead(ch db.Chapter) (db.Chapter, bool) {
	loc := prefaceMarkerRe.FindStringIndex(ch.Content)
	if loc == nil {
		return ch, false
	}
	after := strings.TrimSpace(ch.Content[loc[1]:])
	// The preface ends where the apparatus resumes: Carol's runs straight
	// into CONTENTS and ILLUSTRATIONS lists (with the ",," table-cell debris
	// of a PG layout), and the narration read them all (2026-09-22).
	if cut := apparatusMarkerRe.FindStringIndex(after); cut != nil {
		after = strings.TrimSpace(after[:cut[0]])
	}
	// "Introduction" is also a contents-list entry; what follows a real
	// preface marker is prose, not more chapter lines (Oz's contents page).
	if len(strings.Fields(after)) < 20 || looksLikeContentsList(after) {
		return ch, false
	}
	marker := strings.TrimSpace(ch.Content[loc[0]:loc[1]])
	title := marker
	if isAllCaps(title) {
		title = toTitleCase(title)
	}
	out := ch
	out.Title = strings.TrimSuffix(title, ".")
	out.Content = after
	out.WordCount = len(strings.Fields(after))
	// HTML: keep from the marker's heading/paragraph onward when it can be found.
	if idx := strings.Index(strings.ToUpper(out.ContentHTML), strings.ToUpper(marker)); idx >= 0 {
		start := strings.LastIndex(out.ContentHTML[:idx], "<")
		if start < 0 {
			start = idx
		}
		out.ContentHTML = out.ContentHTML[start:]
	}
	return out, true
}

var leadingHeadingRe = regexp.MustCompile(`(?is)^\s*<h[1-6][^>]*>(.*?)</h[1-6]>\s*`)

// stripLeadingBookTitleLines removes a first line that is the book's own name
// from any chapter whose title is not — the running title stamped above the
// real chapter heading.
func stripLeadingBookTitleLines(chapters []db.Chapter, bookTitle string) []db.Chapter {
	for i := range chapters {
		ch := &chapters[i]
		line, rest, found := strings.Cut(ch.Content, "\n")
		if found && isBookTitle(line, bookTitle) && strings.TrimSpace(rest) != "" {
			ch.Content = strings.TrimLeft(rest, "\n")
			ch.WordCount = len(strings.Fields(ch.Content))
			if m := leadingHeadingRe.FindStringSubmatch(ch.ContentHTML); m != nil &&
				isBookTitle(htmlTagRe.ReplaceAllString(m[1], ""), bookTitle) {
				ch.ContentHTML = strings.TrimSpace(ch.ContentHTML[len(m[0]):])
			}
		}
		// Still named after the book (the spine-file path took the running
		// title, or the TOC did)? Its own opening line names the chapter.
		if isBookTitle(ch.Title, bookTitle) {
			if t := chapterTitleFromContent(ch.Content); t != "" {
				ch.Title = t
			}
		}
	}
	return chapters
}

// chapterTitleFromContent reads a chapter heading off the first lines of the
// plain text: a chapter-like line ("CHAPTER I") plus, when the next non-blank
// line is a short all-caps or title-case sub-heading, that line too — the
// same two-line shape the heading splitter produces ("CHAPTER II\n\nJONATHAN
// HARKER'S JOURNAL—continued").
func chapterTitleFromContent(content string) string {
	lines := strings.Split(content, "\n")
	var picked []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			if len(picked) > 0 && len(picked) < 2 {
				continue
			}
			if len(picked) >= 2 {
				break
			}
			continue
		}
		if len(picked) == 0 {
			if !chapterHeadingTextRe.MatchString(l) || len(strings.Fields(l)) > 8 {
				return ""
			}
			picked = append(picked, l)
			continue
		}
		if len(strings.Fields(l)) <= 8 && !strings.HasSuffix(l, ".") && (isAllCaps(l) || l == toTitleCase(l)) {
			picked = append(picked, l)
		}
		break
	}
	return strings.Join(picked, "\n\n")
}

// Running-header/footer removal. Calibre and many publishers stamp a page
// running-head — the book title, or a shortcode like "HH1 - <Title>" — into the
// body of every section. Once tags are stripped it survives as a short line
// repeated at the top (or bottom) of most chapters: Hitchhiker's Guide emits
// "HH1 - Hitchhiker's Guide to the Galaxy" as a line in 35 of its 36 chapters.
// Each such line joined a chunk, was embedded, and got retrieved + CITED as book
// text — PJ saw a Q&A citation that was nothing but the repeated title, which is
// worse than a cosmetic glitch because it's indistinguishable from a real one.
//
// isHeadingOnly (above) only drops a WHOLE chapter that is nothing but its
// heading; a running head that LEADS a content-bearing chapter slips past it, so
// remove it explicitly here.
//
// Detection keys on the single property that separates a running head from a
// real chapter title: the running head is the SAME short line repeated across
// most chapters, whereas chapter titles differ per chapter ("Chapter 1",
// "Chapter 2", …). So a short line recurring in a high fraction of chapters is
// boilerplate — and removing it can never take out a chapter's own
// (per-chapter-distinct) title. Thresholds stay conservative so a short book or
// an incidental repeat never trips it.
const (
	runHeaderMaxWords = 12  // a running head is short; real prose lines run longer
	runHeaderMinChaps = 4   // never trigger on a handful of chapters
	runHeaderFraction = 0.5 // must recur in at least half the chapters
)

// headerQuoteRepl folds smart quotes so "Hitchhiker's" (curly) and
// "Hitchhiker's" (straight) — which appear in the SAME book's running head —
// normalize to one key.
var headerQuoteRepl = strings.NewReplacer("’", "'", "‘", "'", "“", "\"", "”", "\"")

// normHeaderLine trims, folds smart quotes, and collapses internal whitespace so
// running-head variants compare equal.
func normHeaderLine(s string) string {
	return strings.Join(strings.Fields(headerQuoteRepl.Replace(s)), " ")
}

// stripRunningHeaders removes detected running-header/footer lines from every
// chapter's text (the source of chunks/embeddings/citations) and best-effort
// from the reader HTML, then drops any chapter left empty and re-indexes.
func stripRunningHeaders(chapters []db.Chapter) []db.Chapter {
	if len(chapters) < runHeaderMinChaps {
		return chapters
	}
	// Count, per normalized short line, how many DISTINCT chapters contain it.
	chapCount := map[string]int{}
	for _, ch := range chapters {
		seen := map[string]bool{}
		for _, line := range strings.Split(ch.Content, "\n") {
			key := normHeaderLine(line)
			if key == "" || len(strings.Fields(key)) > runHeaderMaxWords {
				continue
			}
			if !seen[key] {
				seen[key] = true
				chapCount[key]++
			}
		}
	}
	threshold := runHeaderMinChaps
	if f := int(float64(len(chapters)) * runHeaderFraction); f > threshold {
		threshold = f
	}
	headers := map[string]bool{}
	for key, n := range chapCount {
		if n >= threshold {
			headers[key] = true
		}
	}
	if len(headers) == 0 {
		return chapters
	}

	out := make([]db.Chapter, 0, len(chapters))
	idx := 0
	for _, ch := range chapters {
		kept := make([]string, 0, 16)
		for _, line := range strings.Split(ch.Content, "\n") {
			if headers[normHeaderLine(line)] {
				continue
			}
			kept = append(kept, line)
		}
		ch.Content = strings.TrimSpace(strings.Join(kept, "\n"))
		if ch.Content == "" {
			continue // became empty once the boilerplate line(s) were removed
		}
		ch.ContentHTML = stripHeaderBlocksFromHTML(ch.ContentHTML, headers)
		ch.WordCount = len(strings.Fields(ch.Content))
		ch.Index = idx
		idx++
		out = append(out, ch)
	}
	return out
}

// stripHeaderBlocksFromHTML removes block elements (<p>, <h1-6>, <div>) whose
// visible text is exactly a detected running header, so the rich reader view
// matches the cleaned plain text. Best-effort and deliberately narrow: it only
// touches a block whose entire (tag-stripped, normalized) text equals a header,
// so it can't eat real prose. The plain-text strip above is the correctness fix
// (chunks/embeddings/citations read Content); this just keeps the display tidy.
func stripHeaderBlocksFromHTML(html string, headers map[string]bool) string {
	if html == "" || len(headers) == 0 {
		return html
	}
	return htmlBlockRe.ReplaceAllStringFunc(html, func(block string) string {
		inner := normHeaderLine(htmlTagRe.ReplaceAllString(block, ""))
		if headers[inner] {
			return ""
		}
		return block
	})
}

// htmlBlockRe matches a single <p>/<h1-6>/<div> … </p> block (non-greedy, no
// nested same-tag block assumed — running heads are leaf blocks).
var htmlBlockRe = regexp.MustCompile(`(?is)<(p|h[1-6]|div)\b[^>]*>.*?</(?:p|h[1-6]|div)>`)

// extractPerSpineFile is the original one-chapter-per-spine-file extraction,
// used when no chapter headings are detected or when the publisher's own file
// split is finer than the headings we can see.
func extractPerSpineFile(r *zip.Reader, pkg opfPackage, manifest map[string]manifestItem, opfDir string, tocTitles map[string]string, bookID int64) ([]db.Chapter, error) {
	var chapters []db.Chapter
	chapterIdx := 0
	for _, itemref := range pkg.Spine.Itemrefs {
		if itemref.Linear == "no" {
			continue
		}
		item, ok := manifest[itemref.IDRef]
		if !ok {
			continue
		}
		if !strings.Contains(item.MediaType, "html") && !strings.Contains(item.MediaType, "xml") {
			continue
		}
		content, err := readZipFile(r, resolvePath(opfDir, item.Href))
		if err != nil {
			continue
		}
		rawHTML := trimGutenbergBoilerplate(string(content))
		text := strings.TrimSpace(htmlToText(rawHTML))
		if len(text) < 20 {
			continue
		}
		title := tocTitles[stripFragment(item.Href)]
		if title == "" {
			title = extractChapterHeading(rawHTML)
		}
		if isHeadingOnly(text, title) {
			continue // heading-only split document — see isHeadingOnly
		}
		if title == "" {
			title = fmt.Sprintf("Chapter %d", chapterIdx+1)
		}
		chapters = append(chapters, db.Chapter{
			BookID:      bookID,
			Index:       chapterIdx,
			Title:       title,
			Src:         item.Href,
			Content:     text,
			ContentHTML: sanitizeHTML(rawHTML),
			WordCount:   len(strings.Fields(text)),
		})
		chapterIdx++
	}
	return chapters, nil
}

func flattenNavPoints(points []navPoint, titles map[string]string, ncxDir string) {
	for _, np := range points {
		src := stripFragment(np.Content.Src)
		if np.Label.Text != "" && src != "" {
			titles[src] = np.Label.Text
		}
		flattenNavPoints(np.Children, titles, ncxDir)
	}
}

func stripFragment(href string) string {
	if i := strings.Index(href, "#"); i >= 0 {
		return href[:i]
	}
	return href
}

func resolvePath(base, href string) string {
	if base == "." || base == "" {
		return href
	}
	return base + "/" + href
}

func readZipFile(r *zip.Reader, name string) ([]byte, error) {
	f, err := findInZip(r, name)
	if err != nil {
		return nil, err
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

var scriptRe = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
var styleRe = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)

// Footnote/superscript artifacts that, once tags are stripped, glue onto the
// preceding word as a false token ("four1", "mizzen mast bc"). We drop their
// CONTENT (not just the tags) from the plain-text/alignment path. Superscripts
// and footnote-reference anchors are ~always footnote markers in prose EPUBs.
var supSubRe = regexp.MustCompile(`(?is)<(sup|sub)\b[^>]*>.*?</(sup|sub)>`)
var noterefRe = regexp.MustCompile(`(?is)<a\b[^>]*(?:epub:type=["'][^"']*note[^"']*["']|href=["']#(?:fn|note|ftn|en|footnote)[^"']*["'])[^>]*>.*?</a>`)

// Unicode space/zero-width chars (mostly from decoded &nbsp; → U+00A0 and
// friends) that Go's \s doesn't match — normalize to a plain space so they
// don't survive as literal whitespace or fuse tokens.
var uniSpaceRe = regexp.MustCompile(`[\x{00A0}\x{2000}-\x{200B}\x{202F}\x{205F}\x{3000}\x{FEFF}]`)
var blockCloseRe = regexp.MustCompile(`(?i)</(p|div|h[1-6]|li|br|tr)>`)
var brRe = regexp.MustCompile(`(?i)<br\s*/?\s*>`)

// safeTagRe matches opening and closing tags we want to KEEP in sanitized HTML.
// Everything not matched gets stripped. We keep: h1-h6, p, em, strong, i, b,
// blockquote, ul, ol, li, br, sup, sub, span (for karaoke word wrapping later).
var safeTagRe = regexp.MustCompile(`(?i)<(/?)(h[1-6]|p|em|strong|i|b|blockquote|ul|ol|li|br|hr|sup|sub|span)(\s[^>]*)?>`)

// divTagRe: <div …> / </div>, rewritten to <p> / </p> by sanitizeHTML.
var divTagRe = regexp.MustCompile(`(?i)^<(/?)div(\s[^>]*)?>$`)

// Wrapper <div>s (a chapter body div around paragraph divs) become nested
// or empty <p>s after the rewrite; these fold them back to one level.
var (
	emptyParaRe     = regexp.MustCompile(`(?i)<p>\s*</p>`)
	openOpenParaRe  = regexp.MustCompile(`(?i)<p>\s*<p>`)
	closeCloseParRe = regexp.MustCompile(`(?i)</p>\s*</p>`)
)

// sanitizeHTML strips unsafe tags from EPUB XHTML while keeping structural
// markup (headings, paragraphs, emphasis, lists). Removes all attributes
// except on span (where we'll later need data- attrs for karaoke anchoring).
func sanitizeHTML(raw string) string {
	// Remove script/style blocks entirely.
	s := scriptRe.ReplaceAllString(raw, "")
	s = styleRe.ReplaceAllString(s, "")

	// Extract body content if present.
	if idx := strings.Index(strings.ToLower(s), "<body"); idx >= 0 {
		if end := strings.Index(s[idx:], ">"); end >= 0 {
			s = s[idx+end+1:]
		}
	}
	if idx := strings.Index(strings.ToLower(s), "</body>"); idx >= 0 {
		s = s[:idx]
	}

	// Walk through and keep only safe tags, stripping attributes on most.
	var out strings.Builder
	out.Grow(len(s))
	i := 0
	for i < len(s) {
		if s[i] != '<' {
			out.WriteByte(s[i])
			i++
			continue
		}
		// Find end of this tag.
		end := strings.IndexByte(s[i:], '>')
		if end < 0 {
			// Malformed tag, skip the '<'.
			out.WriteByte(s[i])
			i++
			continue
		}
		tag := s[i : i+end+1]
		// <div> is the PARAGRAPH element in some publisher EPUBs (Vintage's
		// Gulag Archipelago, Recorded Books' Crime and Punishment: every
		// paragraph a <div>, no <p> anywhere). Dropping it silently, as the
		// whitelist did, left content_html a single run of <span>s and the
		// reader showed the whole chapter as one block (board 11) — while the
		// plain-text path had always treated </div> as a paragraph break.
		// Emit it as <p>; wrapper nesting is collapsed below.
		if m := divTagRe.FindStringSubmatch(tag); m != nil {
			out.WriteString("<" + m[1] + "p>")
			i += end + 1
			continue
		}
		if safeTagRe.MatchString(tag) {
			// Emit the tag but strip attributes (except on self-closing br).
			m := safeTagRe.FindStringSubmatch(tag)
			if m != nil {
				slash := m[1]
				name := strings.ToLower(m[2])
				if name == "br" || name == "hr" {
					// Void elements — emit self-closing, ignore the slash.
					out.WriteString("<" + name + ">")
				} else {
					out.WriteString("<" + slash + name + ">")
				}
			}
		}
		// Unsafe tag: silently dropped (its text content still emits).
		i += end + 1
	}

	result := strings.TrimSpace(out.String())
	// Collapse runs of whitespace (but preserve single newlines for readability).
	result = whitespaceRe.ReplaceAllString(result, " ")
	// Fold the <p> nesting that wrapper <div>s leave behind (see divTagRe).
	for {
		next := emptyParaRe.ReplaceAllString(result, "")
		next = openOpenParaRe.ReplaceAllString(next, "<p>")
		next = closeCloseParRe.ReplaceAllString(next, "</p>")
		if next == result {
			break
		}
		result = next
	}
	return strings.TrimSpace(result)
}

func htmlToText(raw string) string {
	// Drop <head> entirely. Its <title> is not body text, but stripping tags
	// leaves the title STRING behind, so it lands at the top of the chapter and
	// duplicates the <h1> that repeats it — Calibre-converted EPUBs put the same
	// line in both.
	//
	// This was not cosmetic. Hitchhiker's Guide produced 38 chunks that were
	// nothing but "HH1 - Hitchhiker's Guide to the Galaxy" twice over, ALL of
	// them embedded, so Q&A retrieved and cited them as if they were book text.
	// PJ saw exactly that on his phone: a citation rendering as the title
	// repeated rather than any passage. Fabricated citations are worse than a
	// cosmetic reader glitch, because he has no way to tell them from real ones.
	raw = headRe.ReplaceAllString(raw, "")
	// Remove script and style blocks
	raw = scriptRe.ReplaceAllString(raw, "")
	raw = styleRe.ReplaceAllString(raw, "")
	// Drop footnote/superscript marker CONTENT before stripping tags, so it
	// doesn't glue onto the preceding word ("four1", "mizzen mast bc").
	raw = noterefRe.ReplaceAllString(raw, "")
	raw = supSubRe.ReplaceAllString(raw, "")
	// A closing block tag ends a PARAGRAPH — it must not collapse into the
	// same "\n" as a <br> or a source hard-wrap. This single character
	// destroyed paragraph identity on all 53 epubs in the library (surveyed
	// 2026-08-10): TTS cadence read flat, the paragraphs table filled with
	// ~12-word wrapped lines, and the embedding/paragraph-follow paths
	// consumed those fragments. PJ heard it and blamed the AI voice.
	raw = blockCloseRe.ReplaceAllString(raw, "\n\n")
	raw = brRe.ReplaceAllString(raw, "\n")
	// Strip remaining tags
	text := htmlTagRe.ReplaceAllString(raw, "")
	// Decode HTML entities (&nbsp; &amp; &#8217; …) so they don't survive as
	// literal word tokens ("nbsp"), then fold unicode/zero-width spaces.
	text = gohtml.UnescapeString(text)
	text = uniSpaceRe.ReplaceAllString(text, " ")
	// Normalize per PARAGRAPH (blank-line separated), joining each
	// paragraph's wrapped lines with spaces — the old per-line pass dropped
	// empty lines, re-collapsing the paragraph breaks introduced above.
	// Word stream is unchanged; only whitespace moves.
	var paras []string
	for _, para := range strings.Split(text, "\n\n") {
		var kept []string
		for _, line := range strings.Split(para, "\n") {
			line = whitespaceRe.ReplaceAllString(strings.TrimSpace(line), " ")
			if line != "" {
				kept = append(kept, line)
			}
		}
		if len(kept) > 0 {
			paras = append(paras, strings.Join(kept, " "))
		}
	}
	return strings.Join(paras, "\n\n")
}

// minBodyAfterHeading is how much text must remain once a chapter's own heading
// is discounted before it counts as a chapter at all.
const minBodyAfterHeading = 20

// isHeadingOnly reports whether a chapter's text is nothing but its own heading,
// repeated or not.
//
// Calibre splits an EPUB into many small documents and stamps the BOOK title as
// an <h1> in each one, so dozens of "chapters" contain that line and nothing
// else. The existing len(text) < 20 guard does not catch them because the title
// itself is longer than 20 characters — Hitchhiker's Guide yielded 36 such
// chapters out of 72.
//
// These are not harmless empties. Each became a chunk, every chunk was embedded,
// and Q&A retrieved them as citations: PJ saw one rendered on his phone as the
// answer's source instead of book text. A citation that is just the book title
// is indistinguishable from a real one to the reader.
//
// Discounting the heading rather than testing raw length also covers the
// repeated case, so it does not matter whether the title appears once or ten
// times.
func isHeadingOnly(text, title string) bool {
	body := text
	if t := strings.TrimSpace(title); t != "" {
		body = strings.ReplaceAll(body, t, "")
	}
	// Curly and straight apostrophes both occur in the same book; normalising
	// lets one heading string match both spellings.
	for _, variant := range []string{"\u2019", "'"} {
		if t := strings.TrimSpace(title); t != "" {
			body = strings.ReplaceAll(body, strings.ReplaceAll(t, "'", variant), "")
		}
	}
	return len(strings.TrimSpace(body)) < minBodyAfterHeading
}

var headingRe = regexp.MustCompile(`(?is)<h[1-3][^>]*>(.*?)</h[1-3]>`)

type htmlSegment struct {
	title string
	html  string
	lead  bool // content before the first chapter boundary (front matter)
}

// A heading whose text names a chapter: a chapter-word prefix, a bare roman
// numeral, or a bare number. Front-matter/illustration/section headings
// ("Marley's Ghost", "The Project Gutenberg eBook…") don't match, so we only
// split on real chapter boundaries. Preface/foreword/afterword ARE reading
// units the narrator reads (The Selfish Gene's audio opens with both prefaces),
// so they count as boundaries too.
var chapterHeadingTextRe = regexp.MustCompile(`(?i)^\s*((chapter|stave|part|book|letter|canto|act|scene|prologue|epilogue|volume|preface|foreword|afterword)\b|[ivxlcdm]{1,7}\.?\s*$|\d{1,3}\.?\s*$)`)
var anyHeadingRe = regexp.MustCompile(`(?is)<h[1-6][^>]*>(.*?)</h[1-6]>`)
var tagStripRe = regexp.MustCompile(`(?s)<[^>]+>`)

// A chapter boundary found in the concatenated book HTML. title is empty for a
// tagged heading (the segment's first <h1-3> names it, as before) and set for a
// numbered-paragraph title, which no heading tag would recover.
type headingStart struct {
	pos         int
	title       string
	frontMatter bool // preface/foreword/afterword rather than a chapter
}

// splitHTMLByHeadings splits (concatenated) book HTML at each CHAPTER heading.
// Content before the first chapter heading becomes a leading segment (front
// matter). Returns nil when fewer than 2 chapter headings are present, so the
// caller falls back to per-spine-file extraction (unchanged behavior).
//
// This handles modern Project Gutenberg EPUBs that pack several chapters per
// XHTML file and split files mid-chapter (e.g. #75011) — a chapter can span
// file boundaries, which 1-chapter-per-file extraction buried and mislabeled.
//
// When the book has no tagged chapter headings at all, numbered title
// paragraphs ("7. Family planning") are tried instead — see
// numberedParagraphStarts.
func splitHTMLByHeadings(rawHTML string) []htmlSegment {
	starts, chapterKind := taggedHeadingStarts(rawHTML)
	if chapterKind < 2 {
		// No tagged chapter structure. Numbered title paragraphs may carry it;
		// a preface/foreword heading in front of them stays a boundary too.
		if numbered := numberedParagraphStarts(rawHTML); numbered != nil {
			var merged []headingStart
			for _, t := range starts {
				if t.frontMatter {
					merged = append(merged, t)
				}
			}
			merged = append(merged, numbered...)
			sort.Slice(merged, func(i, j int) bool { return merged[i].pos < merged[j].pos })
			starts = merged
		}
	}
	if len(starts) < 2 {
		return nil
	}
	var segs []htmlSegment
	if starts[0].pos > 0 {
		lead := rawHTML[:starts[0].pos]
		segs = append(segs, htmlSegment{title: extractFirstHeading(lead), html: lead, lead: true})
	}
	for i, s := range starts {
		end := len(rawHTML)
		if i+1 < len(starts) {
			end = starts[i+1].pos
		}
		h := rawHTML[s.pos:end]
		title := s.title
		if title == "" {
			title = extractChapterHeading(h)
		}
		segs = append(segs, htmlSegment{title: title, html: h})
	}
	return segs
}

// Front-matter units the narrator reads but which say nothing about how the
// body is divided: a book with two prefaces and no <hN> chapter headings has
// NO tagged chapter structure, and must not be treated as if it had.
var frontMatterHeadingRe = regexp.MustCompile(`(?i)^\s*(preface|foreword|afterword)\b`)

// taggedHeadingStarts finds every <h1-6> whose text names a chapter or a
// front-matter unit, and reports how many are chapter-kind (not front matter).
func taggedHeadingStarts(rawHTML string) ([]headingStart, int) {
	var starts []headingStart
	chapterKind := 0
	for _, m := range anyHeadingRe.FindAllStringSubmatchIndex(rawHTML, -1) {
		inner := strings.TrimSpace(tagStripRe.ReplaceAllString(rawHTML[m[2]:m[3]], ""))
		if !chapterHeadingTextRe.MatchString(inner) {
			continue
		}
		fm := frontMatterHeadingRe.MatchString(inner)
		if !fm {
			chapterKind++
		}
		starts = append(starts, headingStart{pos: m[0], frontMatter: fm})
	}
	return starts, chapterKind
}

// Numbered title paragraphs. Older Calibre conversions (and plenty of
// publisher files) carry no heading tags at all: every chapter title is an
// ordinary <p> — "1. Why are people?", "2. The replicators." — visually a
// heading, structurally prose. The tagged-heading split sees nothing and the
// book falls back to one chapter per spine file, which for The Selfish Gene
// meant thirteen chapters buried inside six file-sized lumps, so the reader
// could not follow a tapped audio chapter to its text.
//
// A numbered paragraph is only trusted as a chapter title when the set of them
// reads like a table of contents that the book then delivers on:
//   - each is followed by at least a chapter's worth of prose before the next
//     one (a contents list — the same titles packed together — fails this, and
//     so does a short numbered list inside a chapter);
//   - there are at least three;
//   - their numbers strictly increase (one stray "1. …" list item mid-book
//     breaks the chain, and the whole heuristic stands down).
//
// Failing any of these returns nil, i.e. exactly the pre-existing behaviour.
var (
	titleBlockRe    = regexp.MustCompile(`(?is)<(?:p|h[1-6])\b[^>]*>(.*?)</(?:p|h[1-6])>`)
	numberedTitleRe = regexp.MustCompile(`^(\d{1,3})\.\s+\S`)
)

const (
	numberedTitleMaxLen     = 80  // a title is short; a numbered prose paragraph is not
	numberedChapterMinWords = 200 // prose that must follow a title for it to head a chapter
	numberedChapterMinCount = 3
)

func numberedParagraphStarts(rawHTML string) []headingStart {
	type cand struct {
		pos, end, num int
		title         string
	}
	var cands []cand
	for _, m := range titleBlockRe.FindAllStringSubmatchIndex(rawHTML, -1) {
		inner := gohtml.UnescapeString(tagStripRe.ReplaceAllString(rawHTML[m[2]:m[3]], " "))
		inner = strings.Join(strings.Fields(inner), " ")
		if inner == "" || len(inner) > numberedTitleMaxLen {
			continue
		}
		nm := numberedTitleRe.FindStringSubmatch(inner)
		if nm == nil {
			continue
		}
		n, err := strconv.Atoi(nm[1])
		if err != nil || n == 0 {
			continue
		}
		cands = append(cands, cand{pos: m[0], end: m[1], num: n, title: strings.TrimRight(inner, ". ")})
	}
	var kept []cand
	for i, c := range cands {
		bodyEnd := len(rawHTML)
		if i+1 < len(cands) {
			bodyEnd = cands[i+1].pos
		}
		body := tagStripRe.ReplaceAllString(rawHTML[c.end:bodyEnd], " ")
		if len(strings.Fields(body)) < numberedChapterMinWords {
			continue
		}
		kept = append(kept, c)
	}
	if len(kept) < numberedChapterMinCount {
		return nil
	}
	for i := 1; i < len(kept); i++ {
		if kept[i].num <= kept[i-1].num {
			return nil
		}
	}
	starts := make([]headingStart, 0, len(kept))
	for _, c := range kept {
		starts = append(starts, headingStart{pos: c.pos, title: c.title})
	}
	return starts
}

var headingLineBreakRe = regexp.MustCompile(`[ \t]*\n[\s]*`)

// extractChapterHeading prefers the first heading that NAMES a chapter
// ("CHAPTER I", "Stave One", "IV.") over whatever heading merely comes first.
// Gutenberg files put the book's own title in an <h2> right before the first
// chapter's heading, so "first heading" titled Dracula's chapter I
// "D R A C U L A" while every later chapter got its "CHAPTER N" line.
func extractChapterHeading(html string) string {
	for _, m := range anyHeadingRe.FindAllStringSubmatch(html, -1) {
		// <br/> inside a heading is a line break in its text ("I." / "A
		// SCANDAL IN BOHEMIA"); keep it so the sub-title survives, and judge
		// the heading by its FIRST line — the whole text "I. A SCANDAL IN
		// BOHEMIA" is not a numeral, and losing to a bare <h3>I.</h3> below it
		// left Sherlock's chapter I titled "I." (server-web, 2026-09-22).
		text := strings.TrimSpace(htmlTagRe.ReplaceAllString(brRe.ReplaceAllString(m[1], "\n"), ""))
		text = headingLineBreakRe.ReplaceAllString(text, "\n\n") // the store's two-line title shape
		first := strings.TrimSpace(strings.SplitN(text, "\n", 2)[0])
		if first != "" && chapterHeadingTextRe.MatchString(first) {
			return text
		}
	}
	return extractFirstHeading(html)
}

// epubTitle is the package's own dc:title (what the colophon repeats).
func epubTitle(pkg opfPackage) string {
	if len(pkg.Metadata.Title) > 0 {
		return strings.TrimSpace(pkg.Metadata.Title[0])
	}
	return ""
}

func extractFirstHeading(html string) string {
	m := headingRe.FindStringSubmatch(html)
	if m == nil {
		return ""
	}
	text := htmlTagRe.ReplaceAllString(m[1], "")
	return strings.TrimSpace(text)
}
