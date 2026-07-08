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

	var chapters []db.Chapter
	chapterIdx := 0

	// Split on chapter headings. nil => no chapter headings detected, so fall
	// back to the original one-chapter-per-spine-file extraction (correct for
	// EPUBs that put one chapter per file or use non-standard chapter titles).
	segments := splitHTMLByHeadings(book.String())
	if segments == nil {
		return extractPerSpineFile(&r.Reader, pkg, manifest, opfDir, tocTitles, bookID)
	}

	for _, seg := range segments {
		text := strings.TrimSpace(htmlToText(seg.html))
		if len(text) < 20 {
			// Skip near-empty front matter / stray heading fragments
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

	return chapters, nil
}

// extractPerSpineFile is the original one-chapter-per-spine-file extraction,
// used as a fallback for EPUBs where no chapter headings are detected.
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
		rawHTML := string(content)
		text := strings.TrimSpace(htmlToText(rawHTML))
		if len(text) < 20 {
			continue
		}
		title := tocTitles[stripFragment(item.Href)]
		if title == "" {
			title = extractFirstHeading(rawHTML)
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
