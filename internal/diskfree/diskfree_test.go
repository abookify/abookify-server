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

func TestCheck_RefusalNumbersDoNotContradict(t *testing.T) {
	// Live case (Plato, 9.2 GB, on a disk with 9.7 GB free): refused because of
	// the reserve, but "needs 9.2 GB and only 9.7 GB is free" told the person
	// there was room. The stated need must include the reserve.
	free := Check(t.TempDir(), 0, "narrate this book").Free
	if free <= Reserve+2<<30 {
		t.Skip("test host too small to stage a below-reserve refusal")
	}
	need := free - Reserve/2 // fits the disk, not the reserve
	v := Check(t.TempDir(), need, "narrate this book")
	if !v.Refuse {
		t.Fatalf("a write that would eat into the reserve must refuse: %+v", v)
	}
	if !strings.Contains(v.Message, "about "+Human(need+Reserve)+" free") {
		t.Errorf("stated need must include the reserve: %q", v.Message)
	}
	if v.Need != need {
		t.Errorf("need_bytes stays the job's own cost: %d != %d", v.Need, need)
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
