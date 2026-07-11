package library

import "testing"

func TestAuthorsCompatible(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"Erich Maria Remarque", "", true},      // one blank → compatible (the #108/#115 case)
		{"", "Erich Maria Remarque", true},      // symmetric
		{"", "", true},                          // both blank
		{"Mary Shelley", "Shelley, Mary", true}, // same author, different form
		{"Bram Stoker", "Kim Newman", false},    // two real, different authors
	}
	for _, c := range cases {
		if got := authorsCompatible(c.a, c.b); got != c.want {
			t.Errorf("authorsCompatible(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// clusterOf returns the ids of the duplicate group containing want, or nil.
func clusterOf(groups []DuplicateGroup, want int64) []int64 {
	for _, g := range groups {
		for _, w := range g.Works {
			if w.ID == want {
				var ids []int64
				for _, x := range g.Works {
					ids = append(ids, x.ID)
				}
				return ids
			}
		}
	}
	return nil
}

func containsID(ids []int64, id int64) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func TestFindDuplicateWorks_BlankAuthorMatchesByTitle(t *testing.T) {
	store := testStoreForLib(t)
	// The real bug: #108 has a full author, #115 is an orphaned re-upload with a
	// blank author. Same title, single real author → suggested as duplicates.
	full, _ := store.CreateWork("All Quiet on the Western Front", "Erich Maria Remarque")
	blank, _ := store.CreateWork("All Quiet on the Western Front", "")

	groups, err := FindDuplicateWorks(store)
	if err != nil {
		t.Fatal(err)
	}
	cluster := clusterOf(groups, full)
	if len(cluster) != 2 || !containsID(cluster, blank) {
		t.Fatalf("expected full=%d + blank=%d clustered as a pair, got %v", full, blank, cluster)
	}
}

func TestFindDuplicateWorks_BlankDoesNotBridgeTwoAuthors(t *testing.T) {
	store := testStoreForLib(t)
	// Same title, TWO genuinely different real authors, plus an ambiguous blank.
	// The blank must not bridge the two real authors into one merge suggestion.
	a, _ := store.CreateWork("Common Title", "Author One")
	b, _ := store.CreateWork("Common Title", "Author Two")
	store.CreateWork("Common Title", "")

	groups, err := FindDuplicateWorks(store)
	if err != nil {
		t.Fatal(err)
	}
	if c := clusterOf(groups, a); c != nil {
		t.Errorf("author-one work should not be in any duplicate group, got %v", c)
	}
	if c := clusterOf(groups, b); c != nil {
		t.Errorf("author-two work should not be in any duplicate group, got %v", c)
	}
}

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
