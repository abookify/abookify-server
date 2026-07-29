// parts-as-chapters replaces an audio work's detected chapters with ONE chapter
// per source file — the structure the publisher actually shipped.
//
// For long-form spoken content there is nothing to detect. Acoustic silence
// detection classifies any pause ≥3 s as a chapter break, which holds for
// audiobooks (a mastering engineer inserts that gap; a narrator reading prose
// never pauses that long mid-chapter) and collapses completely on conversational
// delivery, where a rhetorical pause is the same acoustic event as a section
// break.
//
// Ghosts of the Ostfront — 4 published parts, 5.74 h — came out as 28 "chapters"
// ranging from SIX SECONDS to 99 MINUTES: seventeen fragments crammed into the
// first hour where the host pauses for emphasis, then three enormous blocks
// through the narrative stretches. That is not a tuning problem. There is no
// threshold that separates "paused for effect" from "new section" because
// acoustically they are identical, so the honest structure is the one the author
// published: the parts.
//
//	docker run … go run ./cmd/parts-as-chapters -db ./data/abookify.db -work 79 [-prefix Part] [-dry-run]
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// roman renders 1..20; beyond that fall back to the number.
func roman(n int) string {
	vals := []struct {
		v int
		s string
	}{{10, "X"}, {9, "IX"}, {5, "V"}, {4, "IV"}, {1, "I"}}
	if n <= 0 || n > 20 {
		return fmt.Sprint(n)
	}
	var b strings.Builder
	for _, r := range vals {
		for n >= r.v {
			b.WriteString(r.s)
			n -= r.v
		}
	}
	return b.String()
}

func main() {
	dbPath := flag.String("db", "./data/abookify.db", "SQLite db path")
	workID := flag.Int64("work", 0, "work id")
	prefix := flag.String("prefix", "Part", "chapter title prefix")
	dryRun := flag.Bool("dry-run", false, "report without writing")
	flag.Parse()
	if *workID == 0 {
		log.Fatal("-work is required")
	}

	sq, err := sql.Open("sqlite", "file:"+*dbPath+"?_pragma=busy_timeout(15000)")
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer sq.Close()
	sq.SetMaxOpenConns(1)

	// Audio books in play order. The FIRST one carries the work's chapter list
	// (chapters hang off it on a book-continuous timeline).
	rows, err := sq.Query(`SELECT id, filename, duration FROM books
	                       WHERE work_id = ? AND media_type = 'audio' ORDER BY path`, *workID)
	if err != nil {
		log.Fatal(err)
	}
	type part struct {
		id       int64
		filename string
		dur      float64
	}
	var parts []part
	for rows.Next() {
		var p part
		if err := rows.Scan(&p.id, &p.filename, &p.dur); err != nil {
			log.Fatal(err)
		}
		parts = append(parts, p)
	}
	rows.Close()
	if len(parts) == 0 {
		log.Fatalf("work %d has no audio", *workID)
	}
	for _, p := range parts {
		if p.dur <= 0 {
			log.Fatalf("%s has no duration — probe durations first, or the timeline will be wrong", p.filename)
		}
	}

	host := parts[0].id
	var existing int
	sq.QueryRow(`SELECT count(*) FROM chapters WHERE book_id = ?`, host).Scan(&existing)

	fmt.Printf("work %d: %d audio file(s), replacing %d detected chapter(s) with %d part(s)\n",
		*workID, len(parts), existing, len(parts))
	var acc float64
	for i, p := range parts {
		fmt.Printf("  %-8s %6.1f min  (%s)\n",
			fmt.Sprintf("%s %s", *prefix, roman(i+1)), p.dur/60, p.filename)
		acc += p.dur
	}
	fmt.Printf("  total %.2f h\n", acc/3600)

	if *dryRun {
		fmt.Println("\ndry-run: nothing written")
		return
	}

	tx, err := sq.Begin()
	if err != nil {
		log.Fatal(err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM chapters WHERE book_id = ?`, host); err != nil {
		log.Fatalf("clear chapters: %v", err)
	}
	acc = 0
	for i, p := range parts {
		title := fmt.Sprintf("%s %s", *prefix, roman(i+1))
		if _, err := tx.Exec(`INSERT INTO chapters
		    (book_id, index_num, title, src, content, word_count, start_sec, end_sec, confidence, content_html)
		    VALUES (?, ?, ?, '', '', 0, ?, ?, 1.0, '')`,
			host, i, title, acc, acc+p.dur); err != nil {
			log.Fatalf("insert %s: %v", title, err)
		}
		acc += p.dur
	}
	// Chapter links pointed at boundaries that no longer exist.
	if _, err := tx.Exec(`DELETE FROM chapter_links WHERE work_id = ?`, *workID); err != nil {
		log.Fatalf("clear chapter links: %v", err)
	}
	if _, err := tx.Exec(`UPDATE works SET content_version = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), *workID); err != nil {
		log.Fatalf("stamp: %v", err)
	}
	// The TRANSCRIPT keeps its finer segmentation — it is the only way to move
	// around inside a 90-minute part, and it carries the text — but its segments
	// must stop calling themselves "Chapter N". They are acoustic breaks, not
	// authored chapters, and labelling them as chapters is the same lie the part
	// structure exists to avoid. Relabel by containing part instead.
	var tbID int64
	if err := tx.QueryRow(`SELECT id FROM books WHERE work_id = ? AND format = 'transcript' LIMIT 1`,
		*workID).Scan(&tbID); err == nil && tbID != 0 {
		rows, err := tx.Query(`SELECT index_num, start_sec FROM chapters WHERE book_id = ? ORDER BY index_num`, tbID)
		if err != nil {
			log.Fatalf("read transcript chapters: %v", err)
		}
		type seg struct {
			idx   int
			start float64
		}
		var segs []seg
		for rows.Next() {
			var sg seg
			rows.Scan(&sg.idx, &sg.start)
			segs = append(segs, sg)
		}
		rows.Close()

		// bounds[i] = start of part i on the book-continuous timeline
		bounds := make([]float64, len(parts))
		var a float64
		for i, p := range parts {
			bounds[i] = a
			a += p.dur
		}
		within := map[int]int{}
		for _, sg := range segs {
			pi := 0
			for i, b := range bounds {
				if sg.start >= b {
					pi = i
				}
			}
			within[pi]++
			title := fmt.Sprintf("%s %s · %d", *prefix, roman(pi+1), within[pi])
			if _, err := tx.Exec(`UPDATE chapters SET title = ? WHERE book_id = ? AND index_num = ?`,
				title, tbID, sg.idx); err != nil {
				log.Fatalf("relabel transcript segment %d: %v", sg.idx, err)
			}
		}
		fmt.Printf("  transcript: relabelled %d segment(s) as \"%s N · n\" (kept for in-part navigation)\n",
			len(segs), *prefix)
	}

	if err := tx.Commit(); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\ndone: %d part(s) are now the navigation for work %d\n", len(parts), *workID)
}
