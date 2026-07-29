// library-audit is a READ-ONLY sweep for the data problems that have actually
// bitten this library, rather than the ones that seem plausible in the abstract.
//
// Every check below exists because the condition was found by accident, usually
// hours into unrelated work:
//
//   - For Whom the Bell Tolls had 16.3 h of audio in one work and its ebook in
//     another; neither could align, and it sat that way for months.
//   - The Call of the Wild held two different narrations after a merge, which
//     produced cross-source alignment rows and read 74.1% A→E.
//   - 13 of its 30 Hemingway files were zero bytes — in the source, not the copy.
//   - A split leaves a second transcript behind, because the generated path
//     embeds the work id.
//
// None of these announce themselves. A work with two narrations looks fine in
// the UI; a low coverage number looks like a transcription problem. Reporting is
// the point: the tool fixes nothing and writes nothing.
//
//	docker run … go run ./cmd/library-audit -db ./data/abookify.db -library ./testdata/library
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"

	_ "modernc.org/sqlite"
)

type finding struct {
	severity string // "HIGH" | "MED" | "LOW"
	check    string
	detail   string
}

var findings []finding

func report(sev, check, format string, a ...any) {
	findings = append(findings, finding{sev, check, fmt.Sprintf(format, a...)})
}

func main() {
	dbPath := flag.String("db", "./data/abookify.db", "SQLite db path")
	libRoot := flag.String("library", "./testdata/library", "host library root that /library maps to")
	flag.Parse()

	// file: URI form — mode=ro is only honoured on a URI, and without it the
	// open intermittently fails with SQLITE_CANTOPEN while the server writes.
	sq, err := sql.Open("sqlite", "file:"+*dbPath+"?mode=ro&_pragma=busy_timeout(15000)")
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer sq.Close()
	sq.SetMaxOpenConns(1)

	checkTwoNarrations(sq)
	checkDuplicateTranscripts(sq)
	checkSplitPairs(sq)
	checkUnreadableAudio(sq, *libRoot)
	checkMissingTranscripts(sq)
	checkCrossWorkRefs(sq)
	checkTranscriptionGaps(sq)
	checkBoilerplate(sq)
	checkDirectionalOutliers(sq)

	sort.SliceStable(findings, func(i, j int) bool {
		rank := map[string]int{"HIGH": 0, "MED": 1, "LOW": 2}
		return rank[findings[i].severity] < rank[findings[j].severity]
	})
	fmt.Printf("\n=== library audit: %d finding(s) ===\n\n", len(findings))
	cur := ""
	for _, f := range findings {
		if f.check != cur {
			fmt.Printf("── %s\n", f.check)
			cur = f.check
		}
		fmt.Printf("   [%-4s] %s\n", f.severity, f.detail)
	}
	if len(findings) == 0 {
		fmt.Println("   clean")
	}
	fmt.Println()
}

// Two narrations in one work. A work is the unit alignment pairs over, so this
// silently produces cross-source alignments and depresses directional coverage.
// Audio files of one recording share a parent directory; two directories means
// two recordings were merged.
func checkTwoNarrations(sq *sql.DB) {
	rows, err := sq.Query(`SELECT b.work_id, w.title, b.path, b.origin FROM books b
	                       JOIN works w ON w.id = b.work_id
	                       WHERE b.media_type = 'audio'`)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	dirs := map[int64]map[string]int{}
	titles := map[int64]string{}
	kind := map[int64]map[string]bool{}
	for rows.Next() {
		var id int64
		var title, p, origin string
		rows.Scan(&id, &title, &p, &origin)
		titles[id] = title
		if dirs[id] == nil {
			dirs[id] = map[string]int{}
			kind[id] = map[string]bool{}
		}
		dirs[id][path.Dir(p)]++
		if strings.HasPrefix(origin, "tts_") {
			kind[id]["tts"] = true
		} else {
			kind[id]["recording"] = true
		}
	}
	for id, d := range dirs {
		if len(d) > 1 {
			var parts []string
			for k, n := range d {
				parts = append(parts, fmt.Sprintf("%s (%d files)", path.Base(k), n))
			}
			sort.Strings(parts)
			note := "two recordings merged — expect cross-source alignment rows"
			if kind[id]["tts"] && kind[id]["recording"] {
				note = "a TTS generation sits alongside a real narration"
			}
			report("HIGH", "two audio sources in one work",
				"work %d %q: %s (%s)",
				id, trunc(titles[id], 40), strings.Join(parts, " + "), note)
		}
	}
}

// A split leaves a stale transcript behind: the generated path embeds the work
// id, so re-importing after a split creates a second one and the alignment may
// still point at the old.
func checkDuplicateTranscripts(sq *sql.DB) {
	rows, _ := sq.Query(`SELECT b.work_id, w.title, count(*), group_concat(b.path)
	                     FROM books b JOIN works w ON w.id = b.work_id
	                     WHERE b.format = 'transcript' GROUP BY b.work_id HAVING count(*) > 1`)
	defer rows.Close()
	for rows.Next() {
		var id int64
		var title, paths string
		var n int
		rows.Scan(&id, &title, &n, &paths)
		report("HIGH", "duplicate transcripts",
			"work %d %q has %d transcripts: %s", id, trunc(title, 40), n, paths)
	}
}

// An audio-only work whose ebook sits in a SEPARATE text-only work: neither can
// align, and nothing surfaces it because both look individually fine.
func checkSplitPairs(sq *sql.DB) {
	type w struct {
		id          int64
		title       string
		audio, text int
	}
	rows, _ := sq.Query(`SELECT w.id, w.title,
	   (SELECT count(*) FROM books b WHERE b.work_id=w.id AND b.media_type='audio'),
	   (SELECT count(*) FROM books b WHERE b.work_id=w.id AND b.format IN ('epub','txt','pdf'))
	   FROM works w`)
	defer rows.Close()
	var all []w
	for rows.Next() {
		var x w
		rows.Scan(&x.id, &x.title, &x.audio, &x.text)
		all = append(all, x)
	}
	for _, a := range all {
		if a.audio == 0 || a.text > 0 {
			continue // needs audio and no text of its own
		}
		for _, t := range all {
			if t.id == a.id || t.text == 0 || t.audio > 0 {
				continue // candidate must be text-only
			}
			if shared := sharedWords(a.title, t.title); len(shared) >= 2 {
				report("HIGH", "audio and its ebook in separate works",
					"work %d %q (%d audio) ↔ work %d %q — cannot align until merged",
					a.id, trunc(a.title, 34), a.audio, t.id, trunc(t.title, 34))
			}
		}
	}
}

// Zero-byte / missing audio. Hemingway shipped 13 of these IN THE SOURCE, and
// the only symptom was a transcription run failing on the first one.
func checkUnreadableAudio(sq *sql.DB, libRoot string) {
	rows, _ := sq.Query(`SELECT b.work_id, w.title, b.filename, b.path, b.duration, b.origin
	                     FROM books b JOIN works w ON w.id = b.work_id
	                     WHERE b.media_type = 'audio'`)
	defer rows.Close()
	bad := map[int64][]string{}
	titles := map[int64]string{}
	ttsNoDur := 0
	for rows.Next() {
		var id int64
		var title, name, p, origin string
		var dur float64
		rows.Scan(&id, &title, &name, &p, &dur, &origin)
		titles[id] = title
		// EVERY tts_kokoro file has duration 0 — the generator never probes it.
		// That is one systemic gap, not a defect per work; counting it per work
		// buried the 22 real recordings that genuinely lack a duration.
		if strings.HasPrefix(origin, "tts_") {
			if dur <= 0 {
				ttsNoDur++
			}
			continue
		}
		// TTS output lives on the `generated` docker volume, which is not
		// visible from here — reporting it "missing" is a false alarm, and an
		// early version of this tool did exactly that for six works. Only the
		// duration is checkable for those.
		if !strings.HasPrefix(p, "/library/") {
			if dur <= 0 {
				bad[id] = append(bad[id], name+" (no duration)")
			}
			continue
		}
		host := strings.Replace(p, "/library/", strings.TrimRight(libRoot, "/")+"/", 1)
		st, err := os.Stat(host)
		switch {
		case err != nil:
			bad[id] = append(bad[id], name+" (missing)")
		case st.Size() == 0:
			bad[id] = append(bad[id], name+" (0 bytes)")
		case dur <= 0:
			bad[id] = append(bad[id], name+" (no duration)")
		}
	}
	if ttsNoDur > 0 {
		report("LOW", "systemic: TTS-generated audio has no duration",
			"%d tts_* files have duration 0 — the generator never probes it; the player has no length to show", ttsNoDur)
	}
	for id, list := range bad {
		sort.Strings(list)
		sev := "MED"
		if len(list) > 5 {
			sev = "HIGH"
		}
		report(sev, "unreadable or zero-length audio",
			"work %d %q: %d file(s) — %s", id, trunc(titles[id], 34), len(list), trunc(strings.Join(list, ", "), 90))
	}
}

func checkMissingTranscripts(sq *sql.DB) {
	rows, _ := sq.Query(`SELECT w.id, w.title,
	    (SELECT count(*) FROM books b WHERE b.work_id=w.id AND b.media_type='audio'),
	    (SELECT coalesce(sum(b.duration),0) FROM books b WHERE b.work_id=w.id AND b.media_type='audio'),
	    (SELECT count(*) FROM books b WHERE b.work_id=w.id AND b.format='transcript')
	    FROM works w`)
	defer rows.Close()
	var total float64
	type row struct {
		id    int64
		title string
		n     int
		dur   float64
	}
	var list []row
	for rows.Next() {
		var id int64
		var title string
		var n, tr int
		var dur float64
		rows.Scan(&id, &title, &n, &dur, &tr)
		if n > 0 && tr == 0 && dur > 0 {
			list = append(list, row{id, title, n, dur})
			total += dur
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].dur > list[j].dur })
	for _, r := range list {
		report("LOW", "audio with no transcript (STT never run)",
			"work %d %q — %d files, %.1f h", r.id, trunc(r.title, 40), r.n, r.dur/3600)
	}
	if total > 0 {
		report("LOW", "audio with no transcript (STT never run)",
			"TOTAL: %.1f h untranscribed", total/3600)
	}
}

// A row asserting a relationship between books that are no longer in the same
// work — the residue of a merge or split.
func checkCrossWorkRefs(sq *sql.DB) {
	var n int
	sq.QueryRow(`SELECT count(*) FROM alignments a
	   JOIN books b1 ON b1.id=a.from_book_id JOIN books b2 ON b2.id=a.to_book_id
	   WHERE b1.work_id != a.work_id OR b2.work_id != a.work_id`).Scan(&n)
	if n > 0 {
		report("HIGH", "dangling cross-work references", "%d alignment row(s) reference books outside their work", n)
	}
	sq.QueryRow(`SELECT count(*) FROM chapter_links c
	   JOIN books b1 ON b1.id=c.audio_book_id JOIN books b2 ON b2.id=c.text_book_id
	   WHERE b1.work_id != c.work_id OR b2.work_id != c.work_id`).Scan(&n)
	if n > 0 {
		report("HIGH", "dangling cross-work references", "%d chapter_link(s) reference books outside their work", n)
	}
	sq.QueryRow(`SELECT count(*) FROM books b LEFT JOIN works w ON w.id=b.work_id WHERE w.id IS NULL`).Scan(&n)
	if n > 0 {
		report("HIGH", "dangling cross-work references", "%d book(s) point at a missing work", n)
	}
}

func checkTranscriptionGaps(sq *sql.DB) {
	rows, _ := sq.Query(`SELECT b.work_id, w.title, b.filename, b.transcription_gaps
	                     FROM books b JOIN works w ON w.id=b.work_id
	                     WHERE b.transcription_gaps NOT IN ('', '[]')`)
	defer rows.Close()
	for rows.Next() {
		var id int64
		var title, name, gaps string
		rows.Scan(&id, &title, &name, &gaps)
		var parsed []map[string]any
		json.Unmarshal([]byte(gaps), &parsed)
		if len(parsed) == 0 {
			continue
		}
		report("MED", "recorded transcription gaps",
			"work %d %q (%s): %d gap(s) — refill with stt-cli -redo-files",
			id, trunc(title, 34), name, len(parsed))
	}
}

var pgLicence = regexp.MustCompile(`(?i)(START OF (?:THE|THIS) PROJECT GUTENBERG EBOOK|END OF (?:THE|THIS) PROJECT GUTENBERG EBOOK|Section 1\. General Terms of Use|End of (?:the )?Project Gutenberg)`)

func checkBoilerplate(sq *sql.DB) {
	rows, _ := sq.Query(`SELECT b.work_id, w.title, b.id, c.content FROM chapters c
	   JOIN books b ON b.id=c.book_id JOIN works w ON w.id=b.work_id
	   WHERE b.format IN ('epub','txt')`)
	defer rows.Close()
	hit := map[int64]string{}
	for rows.Next() {
		var wid, bid int64
		var title, content string
		rows.Scan(&wid, &title, &bid, &content)
		if pgLicence.MatchString(content) {
			hit[bid] = fmt.Sprintf("work %d %q (book %d)", wid, trunc(title, 34), bid)
		}
	}
	for _, d := range hit {
		report("MED", "Project Gutenberg licence text in ebook chapters",
			"%s — re-extract with cmd/reextract-align", d)
	}
}

// A same-language pair where the narration is well backed by the text but the
// text is barely narrated (or vice versa) is usually structural — a collection,
// an abridgement, or TWO NARRATIONS merged. Flagged for a look, not as a defect.
func checkDirectionalOutliers(sq *sql.DB) {
	rows, _ := sq.Query(`SELECT a.id, a.work_id, w.title, a.pairs FROM alignments a
	                     JOIN works w ON w.id = a.work_id`)
	defer rows.Close()
	for rows.Next() {
		var aid, wid int64
		var title, pairs string
		rows.Scan(&aid, &wid, &title, &pairs)
		var p struct {
			EbookWords int `json:"ebook_words"`
			TransWords int `json:"trans_words"`
			Segments   []struct {
				ES int    `json:"es"`
				EE int    `json:"ee"`
				TS int    `json:"ts"`
				TE int    `json:"te"`
				K  string `json:"k"`
			} `json:"segments"`
		}
		if json.Unmarshal([]byte(pairs), &p) != nil || p.EbookWords == 0 || p.TransWords == 0 {
			continue
		}
		var ae, at int
		for _, s := range p.Segments {
			if s.K == "aligned" {
				ae += s.EE - s.ES
				at += s.TE - s.TS
			}
		}
		a2e := float64(at) / float64(p.TransWords)
		e2a := float64(ae) / float64(p.EbookWords)
		// The signature that matters: narration NOT backed by text while the
		// text IS well covered — i.e. words being read that the ebook does not
		// contain, which is what a second merged recording looks like.
		if a2e < 0.75 && e2a > 0.85 {
			report("MED", "directional outlier (possible second narration or wrong edition)",
				"work %d %q align %d: A→E %.3f but E→A %.3f", wid, trunc(title, 34), aid, a2e, e2a)
		}
	}
}

func sharedWords(a, b string) []string {
	norm := func(s string) map[string]bool {
		m := map[string]bool{}
		for _, w := range regexp.MustCompile(`[a-z0-9]+`).FindAllString(strings.ToLower(s), -1) {
			switch w {
			case "the", "a", "an", "unabridged", "of", "and", "v2", "0":
			default:
				if len(w) > 2 {
					m[w] = true
				}
			}
		}
		return m
	}
	ma, mb := norm(a), norm(b)
	var out []string
	for w := range ma {
		if mb[w] {
			out = append(out, w)
		}
	}
	return out
}

func trunc(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
