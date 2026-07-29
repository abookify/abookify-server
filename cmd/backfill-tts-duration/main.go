// backfill-tts-duration probes and fills the duration of already-generated TTS
// audio, which the generator omitted when it registered the books.
//
// Every tts_kokoro row in the library carried duration 0. That is invisible in a
// library listing but underpins the player's scrubber, the book-continuous
// offset maths for a multi-file work, and the waveform — so it surfaces later as
// user-facing breakage rather than as an obvious data error. The generator is
// fixed; this repairs what it already wrote.
//
// TTS output lives on the `generated` docker volume, so run this INSIDE a
// container that mounts it, with ffprobe available:
//
//	docker run --rm -v server_generated-audio:/generated -v "$PWD/data":/app/data \
//	  -v "$PWD":/app -w /app <image-with-ffprobe> go run ./cmd/backfill-tts-duration
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

func probe(path string) (float64, error) {
	out, err := exec.Command("ffprobe", "-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1", path).Output()
	if err != nil {
		return 0, err
	}
	return strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
}

func main() {
	dbPath := flag.String("db", "./data/abookify.db", "SQLite db path")
	dryRun := flag.Bool("dry-run", false, "report without writing")
	flag.Parse()

	sq, err := sql.Open("sqlite", "file:"+*dbPath+"?_pragma=busy_timeout(15000)")
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer sq.Close()
	sq.SetMaxOpenConns(1)

	rows, err := sq.Query(`SELECT id, path, filename FROM books
	                       WHERE media_type = 'audio' AND duration <= 0 ORDER BY id`)
	if err != nil {
		log.Fatal(err)
	}
	type item struct {
		id   int64
		path string
		name string
	}
	var items []item
	for rows.Next() {
		var it item
		rows.Scan(&it.id, &it.path, &it.name)
		items = append(items, it)
	}
	rows.Close()

	var fixed, missing, failed int
	var total float64
	for _, it := range items {
		if _, err := os.Stat(it.path); err != nil {
			missing++
			continue
		}
		d, err := probe(it.path)
		if err != nil || d <= 0 {
			log.Printf("  probe failed: %s (%v)", it.name, err)
			failed++
			continue
		}
		total += d
		fixed++
		if *dryRun {
			fmt.Printf("  would set %-34s %8.1fs\n", it.name, d)
			continue
		}
		if _, err := sq.Exec(`UPDATE books SET duration = ? WHERE id = ?`, d, it.id); err != nil {
			log.Fatalf("update book %d: %v", it.id, err)
		}
	}
	fmt.Printf("\n%d book(s) with duration 0: %d probed (%.2f h), %d file not reachable, %d probe failed\n",
		len(items), fixed, total/3600, missing, failed)
	if *dryRun {
		fmt.Println("dry-run: nothing written")
	}
}
