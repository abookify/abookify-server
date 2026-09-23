package main

import (
	"database/sql"
	"regexp"
	"strconv"
	"strings"
)

// --- card 39: narration rows that begin hours into the book, and titles that
// are narration text -----------------------------------------------------------
//
// Mobile's spot-check (2026-09-23): Dracula's chapter sheet opened at CHAPTER VI
// (2:33:47), Pride and Prejudice at chapter 36 (6.9 h in), Peter Pan at VI —
// the detector's first accepted announcement came late and nothing filled the
// rows before it, although the alignment knew exactly where chapters I–V
// begin. And some rows were titled with what the narrator happened to say
// ("Read by …", "Recorded March 4, 2006", "Xxxviii", "And Fifty-one").

// checkLateFirstRow: a row-bearing narration book (audio anchor or transcript)
// whose first row starts more than lateRowSecs into the audio, while the
// alignment places ebook chapters before it. The stranger's list starts hours
// into the book.
const lateRowSecs = 600.0

func checkLateFirstRow(sq *sql.DB) {
	rows, err := sq.Query(`SELECT a.id, a.work_id, a.from_book_id, a.to_book_id, a.pairs, w.title
		FROM alignments a JOIN works w ON w.id = a.work_id WHERE a.unit = 'word'`)
	if err != nil {
		return
	}
	defer rows.Close()
	bad := 0
	for rows.Next() {
		var id, wid, fb, tb int64
		var pairs, title string
		if err := rows.Scan(&id, &wid, &fb, &tb, &pairs, &title); err != nil {
			continue
		}
		starts := chapterRangeStarts(pairs)
		if len(starts) == 0 {
			continue
		}
		// every row-bearing book of the work that is not the ebook: the
		// anchor audio file(s) and the transcript
		br, err := sq.Query(`SELECT b.id, b.origin, MIN(c.start_sec), COUNT(*) FROM books b JOIN chapters c ON c.book_id = b.id
			WHERE b.work_id = ? AND b.format <> 'epub' AND c.end_sec > c.start_sec GROUP BY b.id`, wid)
		if err != nil {
			continue
		}
		for br.Next() {
			var bid int64
			var origin string
			var first float64
			var n int
			br.Scan(&bid, &origin, &first, &n)
			if first <= lateRowSecs {
				continue
			}
			before := 0
			for _, s := range starts {
				if s < first-30 {
					before++
				}
			}
			if before == 0 {
				continue
			}
			bad++
			report("HIGH", "narration rows begin late",
				"work %d %q: %s book %d's first of %d rows starts at %s, with %d ebook chapter(s) aligned before it — the chapter list opens %s into the book",
				wid, trunc(title, 36), origin, bid, n, hms(first), before, hms(first))
		}
		br.Close()
	}
	if bad == 0 {
		report("LOW", "narration rows begin late", "every row-bearing narration book starts its rows within %.0f min of the audio's start", lateRowSecs/60)
	}
}

// checkNarrationTextTitles: a row title that is what the narrator said rather
// than a chapter name — reader credits, recording notes, a title-cased roman
// numeral, "Chapter N: Chapter M", a spelled-out number, or an obvious
// sentence fragment.
var (
	narrationCreditRe = regexp.MustCompile(`(?i)\b(read by|recorded (on|in|by|march|april|may|june|july|august|september|october|november|december|\d)|librivox|narrated by|this recording|public domain|for more information|end of (chapter|part|book|section))\b`)
	titleCasedRomanRe = regexp.MustCompile(`^(chapter|part)\s+\d+:\s*[XIVLC][xivlc]+\.?$`)
	chapterOfChapter  = regexp.MustCompile(`(?i)^(chapter|part)\s+\d+:\s*(chapter|part)\s+`)
	spelledNumberRe   = regexp.MustCompile(`(?i)^(chapter|part)\s+\d+:\s*(and\s+)?(one|two|three|four|five|six|seven|eight|nine|ten|eleven|twelve|thirteen|fourteen|fifteen|sixteen|seventeen|eighteen|nineteen|twenty|thirty|forty|fifty|sixty|seventy|eighty|ninety|hundred)\b`)
	fragmentTailRe    = regexp.MustCompile(`(?i)\b(mr|mrs|ms|dr|the|a|an|of|to|and|when|if|in|on|at|by|for|with|as|that|which|who|had|was|were|is|are|his|her|their)\.?$`)
)

func looksLikeNarrationText(t string) (string, bool) {
	s := strings.TrimSpace(strings.ReplaceAll(t, "\n", " "))
	switch {
	case narrationCreditRe.MatchString(s):
		return "reader credit / recording note", true
	case titleCasedRomanRe.MatchString(s):
		return "title-cased roman numeral", true
	case chapterOfChapter.MatchString(s):
		return "'Chapter N: Chapter M'", true
	case spelledNumberRe.MatchString(s):
		return "spelled-out number", true
	}
	// "Chapter 36: If Elizabeth, when Mr" — a sentence fragment: five or more
	// words after the label, a comma inside, ending on a function word.
	if i := strings.Index(s, ":"); i > 0 {
		tail := strings.TrimSpace(s[i+1:])
		if len(strings.Fields(tail)) >= 5 && strings.Contains(tail, ",") && fragmentTailRe.MatchString(tail) {
			return "sentence fragment", true
		}
	}
	return "", false
}

func checkNarrationTextTitles(sq *sql.DB) {
	rows, err := sq.Query(`SELECT b.work_id, w.title, b.id, b.origin, c.index_num, c.start_sec, c.title
		FROM chapters c JOIN books b ON b.id = c.book_id JOIN works w ON w.id = b.work_id
		WHERE b.format <> 'epub' AND c.end_sec > c.start_sec ORDER BY b.work_id, b.id, c.index_num`)
	if err != nil {
		return
	}
	defer rows.Close()
	bad, works := 0, map[int64]bool{}
	for rows.Next() {
		var wid, bid int64
		var wtitle, origin, title string
		var idx int
		var start float64
		if err := rows.Scan(&wid, &wtitle, &bid, &origin, &idx, &start, &title); err != nil {
			continue
		}
		why, ok := looksLikeNarrationText(title)
		if !ok {
			continue
		}
		bad++
		works[wid] = true
		report("MED", "row title is narration text",
			"work %d %q: %s book %d row %d at %s is titled %q (%s)", wid, trunc(wtitle, 36), origin, bid, idx, hms(start), trunc(title, 60), why)
	}
	if bad == 0 {
		report("LOW", "row title is narration text", "no narration row is titled with reader credits, recording notes or spoken fragments")
	} else {
		report("MED", "row title is narration text", "%d row(s) across %d work(s) carry narration text as their title", bad, len(works))
	}
}

func hms(sec float64) string {
	s := int(sec + 0.5)
	h, m, r := s/3600, (s%3600)/60, s%60
	if h > 0 {
		return strconv.Itoa(h) + "h " + pad2(m) + "m " + pad2(r) + "s"
	}
	return strconv.Itoa(m) + "m " + pad2(r) + "s"
}

func pad2(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}
