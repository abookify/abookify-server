// Plain text ebook import. Detects chapter boundaries in .txt files using
// common conventions: "CHAPTER N", "Chapter N", blank-line-delimited
// sections, or "Part N". Falls back to a single chapter if no structure
// is detected.
package library

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/pj/abookify/internal/db"
)

var txtChapterRe = regexp.MustCompile(`(?i)^\s*(?:chapter|part|book|section)\s+(\d+|[IVXLC]+)[\s.:—\-]*(.*)$`)
var txtAllCapsHeadingRe = regexp.MustCompile(`^[A-Z][A-Z\s.,;:!?'-]{5,}$`)

// ExtractTXTChapters reads a plain text file and returns chapters with
// content. Chapter boundaries are detected from:
//   - Lines matching "Chapter N" / "Part N" / "Section N" (case-insensitive)
//   - ALL-CAPS headings preceded by blank lines
//   - Falls back to splitting on sequences of 2+ blank lines
//   - Falls back to a single chapter if no structure detected
func ExtractTXTChapters(path string, bookID int64) ([]db.Chapter, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read txt: %w", err)
	}
	text := string(data)
	lines := strings.Split(text, "\n")

	// Pass 1: look for "Chapter N" / "Part N" headings.
	type boundary struct {
		line  int
		title string
	}
	var bounds []boundary
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if m := txtChapterRe.FindStringSubmatch(trimmed); m != nil {
			title := strings.TrimSpace(m[0])
			bounds = append(bounds, boundary{line: i, title: title})
		}
	}

	// Pass 2: if no "Chapter N" found, try ALL-CAPS headings after blank lines.
	if len(bounds) < 2 {
		bounds = nil
		for i, line := range lines {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}
			// Check if previous line(s) were blank.
			prevBlank := i == 0
			if i > 0 && strings.TrimSpace(lines[i-1]) == "" {
				prevBlank = true
			}
			if prevBlank && txtAllCapsHeadingRe.MatchString(trimmed) {
				bounds = append(bounds, boundary{line: i, title: trimmed})
			}
		}
	}

	// Pass 3: if still nothing, split on double-blank-line separators.
	if len(bounds) < 2 {
		bounds = nil
		blankRun := 0
		for i, line := range lines {
			if strings.TrimSpace(line) == "" {
				blankRun++
			} else {
				if blankRun >= 2 {
					bounds = append(bounds, boundary{
						line:  i,
						title: fmt.Sprintf("Section %d", len(bounds)+1),
					})
				}
				blankRun = 0
			}
		}
	}

	// Build chapters from boundaries.
	if len(bounds) < 2 {
		// No structure detected — one big chapter.
		content := strings.TrimSpace(text)
		return []db.Chapter{{
			BookID:    bookID,
			Index:     0,
			Title:     titleFromText(content),
			Content:   content,
			WordCount: len(strings.Fields(content)),
		}}, nil
	}

	var chapters []db.Chapter
	for i, b := range bounds {
		endLine := len(lines)
		if i+1 < len(bounds) {
			endLine = bounds[i+1].line
		}
		content := strings.TrimSpace(strings.Join(lines[b.line:endLine], "\n"))
		if len(strings.Fields(content)) < 5 {
			continue // skip near-empty sections
		}
		chapters = append(chapters, db.Chapter{
			BookID:    bookID,
			Index:     len(chapters),
			Title:     b.title,
			Content:   content,
			WordCount: len(strings.Fields(content)),
		})
	}

	// If we have pre-chapter content (before the first boundary), include it.
	if bounds[0].line > 0 {
		preContent := strings.TrimSpace(strings.Join(lines[:bounds[0].line], "\n"))
		if len(strings.Fields(preContent)) >= 20 {
			// Shift all chapter indices up by 1.
			for i := range chapters {
				chapters[i].Index = i + 1
			}
			pre := db.Chapter{
				BookID:    bookID,
				Index:     0,
				Title:     "Preface",
				Content:   preContent,
				WordCount: len(strings.Fields(preContent)),
			}
			chapters = append([]db.Chapter{pre}, chapters...)
		}
	}

	return chapters, nil
}

func titleFromText(text string) string {
	// Use first non-empty line as title, capped at 80 chars.
	for _, line := range strings.Split(text, "\n") {
		t := strings.TrimSpace(line)
		if t != "" {
			if len(t) > 80 {
				t = t[:80] + "..."
			}
			return t
		}
	}
	return "Untitled"
}
