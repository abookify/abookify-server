package library

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	gohtml "html"
	"io"
	"path"
	"regexp"
	"strings"

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
		return cleanExtractedChapters(perFile), nil
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
		return cleanExtractedChapters(perFile), nil
	}

	return cleanExtractedChapters(chapters), nil
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
//   1. the "editions of this ebook" listing — an intro line, a "click the
//      filenumbers" line, then N "<number> (edition description)" rows; and
//   2. a "Project Gutenberg Editor's Note:" label + its one-sentence note.
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

// cleanExtractedChapters is the single funnel every extraction path returns
// through: it strips the PG apparatus blocks (recomputing word counts, dropping
// any chapter emptied by the strip, re-indexing), then runs the running-header
// pass. Splitting it out keeps the apparatus strip unconditional — the
// running-head pass skips books with too few chapters, but front matter needs
// cleaning regardless of length.
func cleanExtractedChapters(chapters []db.Chapter) []db.Chapter {
	cleaned := make([]db.Chapter, 0, len(chapters))
	idx := 0
	for _, ch := range chapters {
		ch.Content = strings.TrimSpace(stripGutenbergApparatus(ch.Content))
		if ch.Content == "" {
			continue
		}
		ch.WordCount = len(strings.Fields(ch.Content))
		ch.Index = idx
		idx++
		cleaned = append(cleaned, ch)
	}
	return stripRunningHeaders(cleaned)
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
			title = extractFirstHeading(rawHTML)
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
	return result
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
	// Replace block-level tags with newlines
	raw = blockCloseRe.ReplaceAllString(raw, "\n")
	raw = brRe.ReplaceAllString(raw, "\n")
	// Strip remaining tags
	text := htmlTagRe.ReplaceAllString(raw, "")
	// Decode HTML entities (&nbsp; &amp; &#8217; …) so they don't survive as
	// literal word tokens ("nbsp"), then fold unicode/zero-width spaces.
	text = gohtml.UnescapeString(text)
	text = uniSpaceRe.ReplaceAllString(text, " ")
	// Normalize whitespace within lines
	lines := strings.Split(text, "\n")
	var result []string
	for _, line := range lines {
		line = whitespaceRe.ReplaceAllString(strings.TrimSpace(line), " ")
		if line != "" {
			result = append(result, line)
		}
	}
	return strings.Join(result, "\n")
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
}

// A heading whose text names a chapter: a chapter-word prefix, a bare roman
// numeral, or a bare number. Front-matter/illustration/section headings
// ("Preface", "Marley's Ghost", "The Project Gutenberg eBook…") don't match,
// so we only split on real chapter boundaries.
var chapterHeadingTextRe = regexp.MustCompile(`(?i)^\s*((chapter|stave|part|book|letter|canto|act|scene|prologue|epilogue|volume)\b|[ivxlcdm]{1,7}\.?\s*$|\d{1,3}\.?\s*$)`)
var anyHeadingRe = regexp.MustCompile(`(?is)<h[1-6][^>]*>(.*?)</h[1-6]>`)
var tagStripRe = regexp.MustCompile(`(?s)<[^>]+>`)

// splitHTMLByHeadings splits (concatenated) book HTML at each CHAPTER heading.
// Content before the first chapter heading becomes a leading segment (front
// matter). Returns nil when fewer than 2 chapter headings are present, so the
// caller falls back to per-spine-file extraction (unchanged behavior).
//
// This handles modern Project Gutenberg EPUBs that pack several chapters per
// XHTML file and split files mid-chapter (e.g. #75011) — a chapter can span
// file boundaries, which 1-chapter-per-file extraction buried and mislabeled.
func splitHTMLByHeadings(rawHTML string) []htmlSegment {
	var starts []int
	for _, m := range anyHeadingRe.FindAllStringSubmatchIndex(rawHTML, -1) {
		inner := strings.TrimSpace(tagStripRe.ReplaceAllString(rawHTML[m[2]:m[3]], ""))
		if chapterHeadingTextRe.MatchString(inner) {
			starts = append(starts, m[0])
		}
	}
	if len(starts) < 2 {
		return nil
	}
	var segs []htmlSegment
	if starts[0] > 0 {
		lead := rawHTML[:starts[0]]
		segs = append(segs, htmlSegment{title: extractFirstHeading(lead), html: lead})
	}
	for i, s := range starts {
		end := len(rawHTML)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		h := rawHTML[s:end]
		segs = append(segs, htmlSegment{title: extractFirstHeading(h), html: h})
	}
	return segs
}

func extractFirstHeading(html string) string {
	m := headingRe.FindStringSubmatch(html)
	if m == nil {
		return ""
	}
	text := htmlTagRe.ReplaceAllString(m[1], "")
	return strings.TrimSpace(text)
}
