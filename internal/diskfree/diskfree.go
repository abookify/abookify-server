// Package diskfree answers one question before the server writes anything
// large: is there room, and if not, how do we tell a person?
//
// Abookify generates audio onto other people's machines. A self-hosted server
// that quietly fills a laptop until it stops is worse than an error — the
// person will not know the audiobook app is why. So every job that writes
// gigabytes (narration, imports, exports) asks here first, states what it is
// about to use, and refuses in plain words rather than filling the last
// gigabyte. Our own disk-full of 2026-09-22 left a narration file that plays
// for eight minutes and then goes silent; that file is what this prevents.
package diskfree

import "fmt"

const (
	// Reserve is never spent: a write that would leave less than this behind is
	// refused. Enough for the OS and the database to keep working.
	Reserve int64 = 2 << 30 // 2 GiB
	// LowWater is where the UI starts saying "getting low", before anything is
	// refused.
	LowWater int64 = 10 << 30 // 10 GiB
)

// Verdict is the answer for one intended write.
type Verdict struct {
	Free    int64  `json:"free_bytes"`
	Need    int64  `json:"need_bytes"`
	Refuse  bool   `json:"refuse"`
	Low     bool   `json:"low"`
	Message string `json:"message,omitempty"`
}

// Check reports whether a write of `need` bytes under `path` should proceed.
// `doing` is what the person asked for, in their words ("narrate this book",
// "add this sample", "export this book") — the message names that, not a
// mechanism. Free == 0 (unknown filesystem) never refuses: a wrong "no" on an
// unusual mount would be its own bug; the write proceeds and may fail late.
func Check(path string, need int64, doing string) Verdict {
	v := Verdict{Free: Free(path), Need: need}
	if v.Free <= 0 {
		return v
	}
	if need > 0 && v.Free-need < Reserve {
		v.Refuse = true
		// The ask includes the reserve, so the two numbers never contradict
		// each other: "needs 9.2 GB, only 9.7 GB free" reads as room to spare.
		v.Message = fmt.Sprintf("There isn't enough room on this computer to %s. It needs about %s free and there's only %s. Free up some space and try again.",
			doing, Human(need+Reserve), Human(v.Free))
		return v
	}
	if v.Free < LowWater {
		v.Low = true
		if need > 0 {
			v.Message = fmt.Sprintf("This computer is getting low on space — about %s left. %s will use about %s.",
				Human(v.Free), upperFirst(doing), Human(need))
		} else {
			v.Message = fmt.Sprintf("This computer is getting low on space — about %s left. Narration and imports need room to work.", Human(v.Free))
		}
	}
	return v
}

// Human formats bytes the way a person reads them: "1.2 GB", "640 MB".
func Human(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%d MB", n/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return string([]rune(s)[0]-32*boolToRune(s[0] >= 'a' && s[0] <= 'z')) + s[1:]
}

func boolToRune(b bool) rune {
	if b {
		return 1
	}
	return 0
}
