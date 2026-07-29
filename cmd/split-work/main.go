// split-work moves a subset of a work's books out into a new work — the
// inverse of the server's merge.
//
// Merging two works is easy and lossy: once a PG-AI narration is merged into
// the LibriVox work, the fact that they were different editions of different
// recordings is gone, and there is no way back through the API. That costs more
// than tidiness. A work is the unit alignment pairs over, so a work holding two
// narrations gets CROSS-SOURCE alignment rows (one narration's ebook against the
// other's transcript), and its directional coverage is dragged down by counting
// the other recording's words as unaligned — The Call of the Wild read 74.1%
// A→E purely from this.
//
// Rows keyed only by book_id (chapters, paragraphs, chunks, character_mentions,
// summaries) follow their book automatically. Rows that name a work AND a book
// are moved when the book moves. Rows that span the split — an alignment or a
// chapter_link pairing a book on each side — are DELETED, because they assert a
// relationship between two works that no longer share a work; keeping them would
// leave dangling cross-work references. Work-only rows (qa_sessions,
// playback_events, jobs, logs) stay with the original work.
//
//	docker run … go run ./cmd/split-work -db ./data/abookify.db \
//	  -work 86 -books 112410,112411,112416 \
//	  -title "The Call of the Wild (AI-narrated)" -author "Jack London" -dry-run
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

func openDB(path string) (*sql.DB, error) {
	sq, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(15000)")
	if err != nil {
		return nil, err
	}
	sq.SetMaxOpenConns(1)
	return sq, sq.Ping()
}

func main() {
	dbPath := flag.String("db", "./data/abookify.db", "SQLite db path")
	workID := flag.Int64("work", 0, "work to split books OUT of")
	booksCSV := flag.String("books", "", "comma-separated book ids to move to the new work")
	title := flag.String("title", "", "title for the new work")
	author := flag.String("author", "", "author for the new work")
	dryRun := flag.Bool("dry-run", false, "report the plan without writing")
	flag.Parse()

	if *workID == 0 || *booksCSV == "" || *title == "" {
		log.Fatal("-work, -books and -title are required")
	}
	move := map[int64]bool{}
	var moveList []int64
	for _, s := range strings.Split(*booksCSV, ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			log.Fatalf("bad book id %q: %v", s, err)
		}
		move[id] = true
		moveList = append(moveList, id)
	}

	sq, err := openDB(*dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer sq.Close()

	// Every named book must currently belong to the work — otherwise the caller
	// has the wrong ids and would silently steal books from another work.
	for _, id := range moveList {
		var w int64
		if err := sq.QueryRow(`SELECT work_id FROM books WHERE id = ?`, id).Scan(&w); err == sql.ErrNoRows {
			log.Fatalf("book %d does not exist", id)
		} else if err != nil {
			log.Fatalf("book %d: %v", id, err)
		}
		if w != *workID {
			log.Fatalf("book %d belongs to work %d, not %d", id, w, *workID)
		}
	}
	var staying int
	if err := sq.QueryRow(`SELECT count(*) FROM books WHERE work_id = ? AND id NOT IN (`+
		placeholders(len(moveList))+`)`, args(*workID, moveList)...).Scan(&staying); err != nil {
		log.Fatalf("count remaining: %v", err)
	}
	if staying == 0 {
		log.Fatalf("that would move EVERY book off work %d — nothing to split", *workID)
	}
	fmt.Printf("work %d: moving %d book(s) out, %d staying\n", *workID, len(moveList), staying)

	inMove := placeholders(len(moveList))
	crossAlign := queryIDs(sq, `SELECT id FROM alignments WHERE work_id = ?
	  AND ((from_book_id IN (`+inMove+`)) <> (to_book_id IN (`+inMove+`)))`,
		append(args(*workID, moveList), toAny(moveList)...)...)
	crossLinks := queryIDs(sq, `SELECT id FROM chapter_links WHERE work_id = ?
	  AND ((audio_book_id IN (`+inMove+`)) <> (text_book_id IN (`+inMove+`)))`,
		append(args(*workID, moveList), toAny(moveList)...)...)
	fmt.Printf("  cross-boundary rows to DELETE: %d alignment(s) %v, %d chapter_link(s) %v\n",
		len(crossAlign), crossAlign, len(crossLinks), crossLinks)

	if *dryRun {
		fmt.Println("\ndry-run: nothing written")
		return
	}

	tx, err := sq.Begin()
	if err != nil {
		log.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()

	now := time.Now().UTC().Format(time.RFC3339)
	res, err := tx.Exec(`INSERT INTO works (title, author, schema_version, content_version)
	                     VALUES (?, ?, 1, ?)`, *title, *author, now)
	if err != nil {
		log.Fatalf("create work: %v", err)
	}
	newID, _ := res.LastInsertId()

	for _, q := range []struct{ what, sql string }{
		{"books", `UPDATE books SET work_id = ? WHERE id IN (` + inMove + `)`},
		{"sync_data", `UPDATE sync_data SET work_id = ? WHERE audio_book_id IN (` + inMove + `)`},
		{"playback_positions", `UPDATE playback_positions SET work_id = ? WHERE book_id IN (` + inMove + `)`},
		{"bookmarks", `UPDATE bookmarks SET work_id = ? WHERE book_id IN (` + inMove + `)`},
		{"characters", `UPDATE characters SET work_id = ? WHERE book_id IN (` + inMove + `)`},
	} {
		r, err := tx.Exec(q.sql, append([]any{newID}, toAny(moveList)...)...)
		if err != nil {
			log.Fatalf("move %s: %v", q.what, err)
		}
		n, _ := r.RowsAffected()
		fmt.Printf("  moved %-20s %d row(s)\n", q.what, n)
	}

	// Cross-boundary rows go before the same-side rows move, so the delete still
	// sees the pre-move work_id.
	for _, d := range []struct {
		what string
		ids  []int64
	}{{"alignments", crossAlign}, {"chapter_links", crossLinks}} {
		for _, id := range d.ids {
			if _, err := tx.Exec(`DELETE FROM `+d.what+` WHERE id = ?`, id); err != nil {
				log.Fatalf("delete %s %d: %v", d.what, id, err)
			}
		}
		if len(d.ids) > 0 {
			fmt.Printf("  deleted %-18s %d cross-boundary row(s)\n", d.what, len(d.ids))
		}
	}

	for _, q := range []struct{ what, sql string }{
		{"alignments", `UPDATE alignments SET work_id = ? WHERE from_book_id IN (` + inMove + `)`},
		{"chapter_links", `UPDATE chapter_links SET work_id = ? WHERE audio_book_id IN (` + inMove + `)`},
	} {
		r, err := tx.Exec(q.sql, append([]any{newID}, toAny(moveList)...)...)
		if err != nil {
			log.Fatalf("move %s: %v", q.what, err)
		}
		n, _ := r.RowsAffected()
		fmt.Printf("  moved %-20s %d row(s)\n", q.what, n)
	}

	// A display override pointing at a book that just left is now dangling.
	// ResolveDisplayText/Audio fall back to authority order when it is 0.
	if _, err := tx.Exec(`UPDATE works SET display_text_book_id = 0
	                      WHERE id = ? AND display_text_book_id IN (`+inMove+`)`,
		append([]any{*workID}, toAny(moveList)...)...); err != nil {
		log.Fatalf("clear display_text: %v", err)
	}
	if _, err := tx.Exec(`UPDATE works SET display_audio_book_id = 0
	                      WHERE id = ? AND display_audio_book_id IN (`+inMove+`)`,
		append([]any{*workID}, toAny(moveList)...)...); err != nil {
		log.Fatalf("clear display_audio: %v", err)
	}

	// Both works changed, so both need a fresh stamp for mobile's update-check.
	if _, err := tx.Exec(`UPDATE works SET content_version = ?, updated_at = CURRENT_TIMESTAMP
	                      WHERE id IN (?, ?)`, now, *workID, newID); err != nil {
		log.Fatalf("stamp versions: %v", err)
	}

	if err := tx.Commit(); err != nil {
		log.Fatalf("commit: %v", err)
	}
	fmt.Printf("\nsplit done: new work %d %q — re-align both works next\n", newID, *title)
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func toAny(ids []int64) []any {
	out := make([]any, len(ids))
	for i, v := range ids {
		out[i] = v
	}
	return out
}

func args(first int64, ids []int64) []any {
	return append([]any{first}, toAny(ids)...)
}

func queryIDs(sq *sql.DB, q string, a ...any) []int64 {
	rows, err := sq.Query(q, a...)
	if err != nil {
		log.Fatalf("query: %v\n%s", err, q)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			log.Fatalf("scan: %v", err)
		}
		out = append(out, id)
	}
	return out
}
