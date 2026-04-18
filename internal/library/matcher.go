package library

import (
	"log"
	"regexp"
	"strings"
	"unicode"

	"github.com/pj/abookify/internal/db"
)

var nonAlphaNum = regexp.MustCompile(`[^a-z0-9]+`)

// normalize reduces a string to lowercase alphanumeric for fuzzy matching.
func normalize(s string) string {
	s = strings.ToLower(s)
	s = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsSpace(r) {
			return r
		}
		return ' '
	}, s)
	s = nonAlphaNum.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// matchKey produces a key for matching from title (and optionally author).
// It strips common suffixes like "64kb", codec info, chapter numbers, etc.
func matchKey(title, author string) string {
	t := normalize(title)

	// Remove common audiobook filename noise
	for _, noise := range []string{"64kb", "128kb", "librivox", "gutenberg"} {
		t = strings.ReplaceAll(t, noise, "")
	}
	t = strings.TrimSpace(t)

	// If author is known, prepend it
	if author != "" {
		a := normalize(author)
		return a + " " + t
	}
	return t
}

// MatchAndCreateWorks looks at unassigned books, groups them by likely title/author,
// and creates Work entries linking audio files with text files.
func MatchAndCreateWorks(store *db.Store) error {
	books, err := store.UnassignedBooks()
	if err != nil {
		return err
	}

	if len(books) == 0 {
		return nil
	}

	// Group books by their parent directory (audio chapters share a dir)
	// and by normalized metadata for matching across formats.
	type candidate struct {
		title     string // best title found
		author    string // best author found
		audioIDs  []int64
		textIDs   []int64
	}

	// First pass: group audio files by directory, text files individually
	dirGroups := map[string]*candidate{}
	var textBooks []db.Book

	for _, b := range books {
		if b.MediaType == "audio" {
			// Group by parent directory
			parts := strings.Split(b.Path, "/")
			dir := strings.Join(parts[:len(parts)-1], "/")

			c, ok := dirGroups[dir]
			if !ok {
				c = &candidate{}
				dirGroups[dir] = c
			}
			c.audioIDs = append(c.audioIDs, b.ID)

			// Use the best metadata we find (prefer album tag for audiobooks)
			if b.Album != "" && (c.title == "" || len(b.Album) > len(c.title)) {
				c.title = b.Album
			} else if b.Title != "" && c.title == "" {
				c.title = b.Title
			}
			if b.Author != "" {
				c.author = b.Author
			}
		} else {
			textBooks = append(textBooks, b)
		}
	}

	// Build a lookup of text books by normalized title
	type textEntry struct {
		book db.Book
		key  string
	}
	var textEntries []textEntry
	for _, b := range textBooks {
		key := normalize(b.Title + " " + b.Author)
		textEntries = append(textEntries, textEntry{book: b, key: key})
	}

	// For each audio group, try to find a matching text file
	matchedTextIDs := map[int64]bool{}

	for _, c := range dirGroups {
		audioKey := normalize(c.title + " " + c.author)

		// Try to find a matching text book.
		// Require significant title overlap — not just common words.
		audioTitleNorm := normalize(c.title)
		bestScore := 0
		bestIdx := -1
		for i, te := range textEntries {
			if matchedTextIDs[te.book.ID] {
				continue
			}
			// Score based on title-to-title overlap (not author mixed in)
			textTitleNorm := normalize(te.book.Title)
			titleScore := overlapScore(audioTitleNorm, textTitleNorm)
			// Also check full key (title + author)
			fullScore := overlapScore(audioKey, te.key)

			// Require at least 3 overlapping title words, or very high full overlap
			if titleScore >= 3 || (titleScore >= 2 && fullScore >= 4) {
				if titleScore > bestScore {
					bestScore = titleScore
					bestIdx = i
				}
			}
		}

		if bestIdx >= 0 {
			c.textIDs = append(c.textIDs, textEntries[bestIdx].book.ID)
			matchedTextIDs[textEntries[bestIdx].book.ID] = true

			// Prefer EPUB metadata for title/author (usually cleaner)
			te := textEntries[bestIdx].book
			if te.Title != "" {
				c.title = te.Title
			}
			if te.Author != "" {
				c.author = te.Author
			}
		}

		// Create the work
		workTitle := c.title
		if workTitle == "" {
			workTitle = "Unknown Title"
		}

		workID, err := store.CreateWork(workTitle, c.author)
		if err != nil {
			return err
		}

		allIDs := append(c.audioIDs, c.textIDs...)
		if err := store.AssignBooksToWork(workID, allIDs); err != nil {
			return err
		}

		log.Printf("created work: %q by %q (%d audio, %d text files)",
			workTitle, c.author, len(c.audioIDs), len(c.textIDs))
	}

	// Create works for unmatched text files. If a work with the same
	// normalized title already exists (e.g. the EPUB was already created as
	// a work, now we see the MOBI), assign this book to the existing work
	// instead of creating a duplicate. This is how multi-ebook works form.
	existingWorks, _ := store.ListWorks()
	existingByKey := map[string]int64{}
	for _, w := range existingWorks {
		key := normalize(w.Title + " " + w.Author)
		existingByKey[key] = w.ID
	}

	for _, te := range textEntries {
		if matchedTextIDs[te.book.ID] {
			continue
		}

		title := te.book.Title
		if title == "" {
			title = te.book.Filename
		}

		// Check if a work with this title+author already exists.
		key := normalize(title + " " + te.book.Author)
		if existingID, ok := existingByKey[key]; ok {
			if err := store.AssignBooksToWork(existingID, []int64{te.book.ID}); err != nil {
				return err
			}
			log.Printf("added %q (%s) to existing work %d", te.book.Filename, te.book.Format, existingID)
			continue
		}

		workID, err := store.CreateWork(title, te.book.Author)
		if err != nil {
			return err
		}
		if err := store.AssignBooksToWork(workID, []int64{te.book.ID}); err != nil {
			return err
		}
		existingByKey[key] = workID // track for subsequent matches

		log.Printf("created work (text only): %q by %q", title, te.book.Author)
	}

	return nil
}

// overlapScore counts shared words between two normalized strings.
func overlapScore(a, b string) int {
	wordsA := strings.Fields(a)
	wordsB := map[string]bool{}
	for _, w := range strings.Fields(b) {
		if len(w) > 2 { // skip tiny words like "or", "by", "the"
			wordsB[w] = true
		}
	}
	score := 0
	for _, w := range wordsA {
		if len(w) > 2 && wordsB[w] {
			score++
		}
	}
	return score
}
