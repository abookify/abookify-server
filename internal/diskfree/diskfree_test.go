package diskfree

import (
	"strings"
	"testing"
)

func TestCheck_WordsAPersonCanAct(t *testing.T) {
	// Free is read from the real filesystem; exercise the message shapes via
	// a temp dir (never refuses in CI unless the runner really is full).
	v := Check(t.TempDir(), 0, "narrate this book")
	if v.Refuse {
		t.Skip("test host is genuinely out of space")
	}
	if v.Free <= 0 {
		t.Skip("free space unknown on this filesystem")
	}
	// Force a refusal by asking for more than exists.
	v = Check(t.TempDir(), v.Free+1<<30, "narrate this book")
	if !v.Refuse {
		t.Fatalf("asking for more than free must refuse: %+v", v)
	}
	for _, want := range []string{"isn't enough room", "narrate this book", "GB", "Free up some space"} {
		if !strings.Contains(v.Message, want) {
			t.Errorf("refusal message %q lacks %q", v.Message, want)
		}
	}
	if strings.Contains(v.Message, "bytes") || strings.Contains(strings.ToLower(v.Message), "statfs") {
		t.Errorf("message must not read like a mechanism: %q", v.Message)
	}
}

func TestHuman(t *testing.T) {
	cases := map[int64]string{1<<30 + 214748365: "1.2 GB", 640 << 20: "640 MB", 3 << 10: "3 KB", 12: "12 bytes"}
	for n, want := range cases {
		if got := Human(n); got != want {
			t.Errorf("Human(%d) = %q, want %q", n, got, want)
		}
	}
}
