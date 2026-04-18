package library

import "testing"

func TestNormalizeTitle(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Frankenstein", "frankenstein"},
		{"Frankenstein; or, the modern prometheus", "frankenstein-or-the-modern-prometheus"},
		{"The Great Gatsby", "great-gatsby"},
		{"A Tale of Two Cities", "tale-of-two-cities"},
		{"Pride and Prejudice (Unabridged)", "pride-and-prejudice"},
		{"War and Peace [EPUB]", "war-and-peace"},
		{"Book Title Volume 1", "book-title"},
		{"My Book — Audiobook", "my-book"},
		// Empty
		{"", ""},
	}
	for _, c := range cases {
		got := normalizeTitle(c.in)
		if got != c.want {
			t.Errorf("normalizeTitle(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeAuthor(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Mary Shelley", "shelley"},
		{"Shelley, Mary", "shelley"},
		{"Mary Wollstonecraft Shelley", "shelley"},
		{"Dickens, Charles", "dickens"},
		{"", ""},
		{"H. P. Lovecraft", "lovecraft"},
	}
	for _, c := range cases {
		got := normalizeAuthor(c.in)
		if got != c.want {
			t.Errorf("normalizeAuthor(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeWorkKey_DuplicateMatch(t *testing.T) {
	// Both should produce the same dedup key.
	k1 := normalizeWorkKey("Frankenstein; or, the modern prometheus", "Mary Shelley")
	k2 := normalizeWorkKey("Frankenstein — Or, The Modern Prometheus (Unabridged)", "Shelley, Mary Wollstonecraft")
	if k1 != k2 {
		t.Errorf("expected same key for duplicate variants, got:\n  %q\n  %q", k1, k2)
	}
	if k1 == "" {
		t.Error("key should not be empty for a real book")
	}
}

func TestNormalizeWorkKey_DifferentAuthors(t *testing.T) {
	// Same title but different authors should NOT dedup.
	k1 := normalizeWorkKey("Dracula", "Bram Stoker")
	k2 := normalizeWorkKey("Dracula", "Kim Newman")
	if k1 == k2 {
		t.Errorf("different authors should not produce same key: %q", k1)
	}
}
