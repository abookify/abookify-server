// LLM fallback for chapter titles the narrator-pattern extractor
// couldn't recover. Reads the first ~300 words of body text from each
// bare "Chapter N" / "Part N" row and asks the configured LLM to
// propose a short subtitle. Updates the chapter row's title in-place.
//
// Skips chapters that already have a subtitle, "Prologue"/"Foreword"
// style titles, and any chapter whose content is too short to label
// meaningfully (< 30 words).
package library

import (
	"fmt"
	"log"
	"regexp"
	"strings"

	"github.com/pj/abookify/internal/db"
	"github.com/pj/abookify/internal/llm"
)

// bareChapterTitleRe matches "Chapter N" / "Part N" / "Book N" with
// nothing after the number. These are the rows we'll send to the LLM —
// anything with a ":" subtitle came back clean from the heuristic and
// stays as-is.
var bareChapterTitleRe = regexp.MustCompile(`^(Chapter|Part|Book)\s+\d+\s*$`)

// LabelMissingChapterTitles asks the configured LLM to propose
// subtitles for any "Chapter N" rows the narrator-pattern extractor
// couldn't title. Operates across every text book in the work.
//
// Cheap: typically 1-3 calls per work (audiobooks with 30+ chapters
// usually have at most a handful where the narrator didn't say a
// title). Returns silently if no LLM is configured.
func LabelMissingChapterTitles(store *db.Store, client *llm.Client, workID int64) error {
	if client == nil {
		return nil
	}
	work, err := store.GetWork(workID)
	if err != nil || work == nil {
		return err
	}
	labeled := 0
	for _, b := range work.TextFiles {
		// Only chapters from narrator-pattern detection have bare
		// "Chapter N" titles to repair. EPUB chapters carry the
		// publisher's title and don't need this.
		if b.Format != "transcript" {
			continue
		}
		chapters, err := store.ListChapters(b.ID)
		if err != nil {
			continue
		}
		for _, ch := range chapters {
			if !bareChapterTitleRe.MatchString(strings.TrimSpace(ch.Title)) {
				continue
			}
			full, err := store.GetChapterContent(b.ID, ch.Index)
			if err != nil || full == nil {
				continue
			}
			snippet := firstWords(full.Content, 300)
			if len(strings.Fields(snippet)) < 30 {
				continue
			}
			label, err := askLLMForChapterTitle(client, ch.Title, snippet)
			if err != nil {
				log.Printf("llm-titles: %s on book %d: %v", ch.Title, b.ID, err)
				continue
			}
			if label == "" {
				continue
			}
			newTitle := ch.Title + ": " + label
			if err := store.UpdateChapterTitle(b.ID, ch.Index, newTitle); err != nil {
				log.Printf("llm-titles: update %s: %v", ch.Title, err)
				continue
			}
			log.Printf("llm-titles: %s → %q (book %d)", ch.Title, newTitle, b.ID)
			labeled++
		}
	}
	if labeled > 0 {
		log.Printf("llm-titles: labeled %d chapters for work %d", labeled, workID)
	}
	return nil
}

// firstWords returns the first n whitespace-separated words of s. If s
// has fewer than n words, returns s unchanged.
func firstWords(s string, n int) string {
	fields := strings.Fields(s)
	if len(fields) <= n {
		return strings.Join(fields, " ")
	}
	return strings.Join(fields[:n], " ")
}

// askLLMForChapterTitle prompts the model for a short subtitle. The
// prompt is deliberately strict — single line, no quotes, no preamble
// — so we can use the raw response as a title without parsing. Returns
// "" if the model declines (no clear subject in the snippet).
func askLLMForChapterTitle(client *llm.Client, current, snippet string) (string, error) {
	system := "You label audiobook chapters. The narrator's announcement for this chapter didn't include a subtitle. Read the opening text and propose a short, specific subtitle (1-6 words) that captures the chapter's main subject. Use the same noun-phrase style as a book's table of contents (no full sentences). Respond with ONLY the subtitle on one line, no quotes, no preamble, no period. If the snippet has no clear theme, respond with exactly: (none)"
	user := fmt.Sprintf("Current title: %s\n\nOpening text:\n%s", current, snippet)
	resp, err := client.Complete(llm.CompletionRequest{
		System:      system,
		Messages:    []llm.Message{{Role: "user", Content: user}},
		MaxTokens:   30,
		Temperature: 0.2,
	})
	if err != nil {
		return "", err
	}
	out := strings.TrimSpace(resp.Content)
	out = strings.Trim(out, `"'`)
	out = strings.TrimSuffix(out, ".")
	out = strings.TrimSpace(out)
	if out == "" || strings.EqualFold(out, "(none)") || strings.EqualFold(out, "none") {
		return "", nil
	}
	// Sanity guard against runaway responses — if the model went past
	// the requested ~6 words despite the instructions, truncate
	// rather than poisoning the TOC.
	if fields := strings.Fields(out); len(fields) > 8 {
		out = strings.Join(fields[:8], " ")
	}
	return out, nil
}
