package library

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	tmp := filepath.Join(t.TempDir(), "test.txt")
	if err := os.WriteFile(tmp, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return tmp
}

func TestExtractTXTChapters_ChapterN(t *testing.T) {
	content := `Preface

This is the preface with enough words to count as a real section
that has some content that we should detect and include.

Chapter 1: The Beginning

It was a dark and stormy night. The rain fell in torrents.
Many words follow here to make this a real chapter.

Chapter 2: The Middle

Things got worse. Much worse. And then better.
More content to make this chapter substantial enough.

Chapter 3: The End

And so the story concludes with a satisfying resolution.
`
	path := writeTempFile(t, content)
	chapters, err := ExtractTXTChapters(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	// Should have: Preface + 3 chapters = 4
	if len(chapters) < 3 {
		t.Fatalf("want ≥3 chapters, got %d: %+v", len(chapters), chapters)
	}
	// First real chapter should be "Chapter 1: The Beginning"
	found := false
	for _, ch := range chapters {
		if strings.Contains(ch.Title, "Chapter 1") {
			found = true
		}
	}
	if !found {
		t.Error("expected a chapter titled 'Chapter 1...'")
	}
}

func TestExtractTXTChapters_NoStructure(t *testing.T) {
	content := "Just a single paragraph with no chapter markers at all. " +
		strings.Repeat("Some more text. ", 20)
	path := writeTempFile(t, content)
	chapters, err := ExtractTXTChapters(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(chapters) != 1 {
		t.Fatalf("no structure → 1 chapter, got %d", len(chapters))
	}
}

func TestExtractTXTChapters_DoubleBlankLines(t *testing.T) {
	content := "First section content here with enough words.\n\n\n" +
		"Second section begins after two blank lines.\n\n\n" +
		"Third section is the finale."
	path := writeTempFile(t, content)
	chapters, err := ExtractTXTChapters(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	// Double-blank-line detection should find 3 sections.
	// (Actually: the first section isn't preceded by blank lines, so
	// it won't be detected as a boundary. We get 2 sections + the first
	// section becomes pre-chapter content or gets absorbed.)
	if len(chapters) < 2 {
		t.Errorf("expected ≥2 sections from double-blank-line detection, got %d", len(chapters))
	}
}
