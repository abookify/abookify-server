// cast-test runs the Go cast heuristic on a book's chapters (for parity checks
// vs the Python prototype). Read-only.
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"

	_ "modernc.org/sqlite"

	"github.com/pj/abookify/internal/db"
	lib "github.com/pj/abookify/internal/library"
)

func main() {
	dbPath := flag.String("db", "./data/abookify.db", "sqlite db")
	bookID := flag.Int64("book", 0, "text book id")
	n := flag.Int("n", 15, "top N")
	flag.Parse()

	sq, err := sql.Open("sqlite", "file:"+*dbPath+"?mode=ro")
	if err != nil {
		log.Fatal(err)
	}
	defer sq.Close()
	rows, err := sq.Query("SELECT index_num, title, content, word_count FROM chapters WHERE book_id=? ORDER BY index_num", *bookID)
	if err != nil {
		log.Fatal(err)
	}
	var chapters []db.Chapter
	for rows.Next() {
		var ch db.Chapter
		if err := rows.Scan(&ch.Index, &ch.Title, &ch.Content, &ch.WordCount); err != nil {
			log.Fatal(err)
		}
		chapters = append(chapters, ch)
	}
	cast := lib.ExtractCastHeuristic(chapters, 3)
	fmt.Printf("book %d: %d chapters -> %d candidates. top %d:\n", *bookID, len(chapters), len(cast), *n)
	for i, c := range cast {
		if i >= *n {
			break
		}
		flag := ""
		if c.IsPlace {
			flag = "  (place?)"
		}
		fmt.Printf("%2d  %-14s mentions=%-4d total=%-4d dict=%v%s\n", i+1, c.Name, c.Mentions, c.Total, c.InDict, flag)
	}
}
