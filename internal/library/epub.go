package library

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
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

	// Walk spine in reading order
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

		// Only process XHTML/HTML content
		if !strings.Contains(item.MediaType, "html") && !strings.Contains(item.MediaType, "xml") {
			continue
		}

		filePath := resolvePath(opfDir, item.Href)
		content, err := readZipFile(&r.Reader, filePath)
		if err != nil {
			continue
		}

		rawHTML := string(content)

		// Extract text from HTML (for search, alignment, word count)
		text := htmlToText(rawHTML)
		text = strings.TrimSpace(text)

		if len(text) < 20 {
			// Skip near-empty pages (title pages, etc.)
			continue
		}

		// Sanitized HTML for rich-text rendering in the reader
		richHTML := sanitizeHTML(rawHTML)

		// Find title from NCX or extract from content
		title := ""
		baseSrc := stripFragment(item.Href)
		if t, ok := tocTitles[baseSrc]; ok {
			title = t
		}
		if title == "" {
			title = extractFirstHeading(rawHTML)
		}
		if title == "" {
			title = fmt.Sprintf("Chapter %d", chapterIdx+1)
		}

		wordCount := len(strings.Fields(text))

		chapters = append(chapters, db.Chapter{
			BookID:      bookID,
			Index:       chapterIdx,
			Title:       title,
			Src:         item.Href,
			Content:     text,
			ContentHTML: richHTML,
			WordCount:   wordCount,
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

func htmlToText(html string) string {
	// Remove script and style blocks
	html = scriptRe.ReplaceAllString(html, "")
	html = styleRe.ReplaceAllString(html, "")
	// Replace block-level tags with newlines
	html = blockCloseRe.ReplaceAllString(html, "\n")
	html = brRe.ReplaceAllString(html, "\n")
	// Strip remaining tags
	text := htmlTagRe.ReplaceAllString(html, "")
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

var headingRe = regexp.MustCompile(`(?i)<h[1-3][^>]*>(.*?)</h[1-3]>`)

func extractFirstHeading(html string) string {
	m := headingRe.FindStringSubmatch(html)
	if m == nil {
		return ""
	}
	text := htmlTagRe.ReplaceAllString(m[1], "")
	return strings.TrimSpace(text)
}
