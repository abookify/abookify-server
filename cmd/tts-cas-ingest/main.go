// tts-cas-ingest: one-shot for deploying the content-addressed store onto a
// library with freshly generated editions (the 11-book showcase run): for
// each TTS chapter file WITHOUT a .cas sidecar, recompute today's content
// key from the CURRENT text and ingest the file into the CAS + stamp the
// sidecar — ONLY when the edition was generated from that exact text
// (guarded: refuses books whose text book can't be resolved). Editions of
// unverified currency (pre-migration audio) are deliberately NOT stamped:
// reading them as stale is the system being right.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/pj/abookify/internal/db"
	"github.com/pj/abookify/internal/library"
)

func main() {
	dbPath := flag.String("db", "./data/abookify.db", "")
	generated := flag.String("generated", "", "generated dir (host path)")
	work := flag.Int64("work", 0, "restrict to one work (0=all)")
	flag.Parse()
	if *generated == "" {
		log.Fatal("-generated required")
	}
	store, err := db.Open(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	books, err := store.ListBooksByFormat("mp3")
	if err != nil {
		log.Fatal(err)
	}
	stamped, skipped := 0, 0
	for _, b := range books {
		if b.Origin != "tts_kokoro" || (*work != 0 && b.WorkID != *work) {
			continue
		}
		base := filepath.Base(filepath.Dir(b.Path))
		var textID int64
		var voice string
		if n, _ := fmt.Sscanf(base, "tts-book-%d-", &textID); n != 1 {
			continue
		}
		if i := strings.Index(base, fmt.Sprintf("tts-book-%d-", textID)); i >= 0 {
			voice = strings.ReplaceAll(base[len(fmt.Sprintf("tts-book-%d-", textID)):], "-", "_")
		}
		host := filepath.Join(*generated, base, filepath.Base(b.Path))
		if _, err := os.Stat(host + ".cas"); err == nil {
			continue // already stamped
		}
		var idx int
		fmt.Sscanf(filepath.Base(b.Path), "chapter-%d.", &idx)
		ch, err := store.GetChapterContent(textID, idx)
		if err != nil || ch == nil {
			skipped++
			continue
		}
		segs := library.PreprocessForTTSSegments(ch.Title, ch.Content)
		var texts []string
		for _, s := range segs {
			texts = append(texts, s.Text)
		}
		key := library.TTSContentKey(strings.Join(texts, "\n\n"), voice, 1100, 500)
		wd := filepath.Join(*generated, "work", "ingest")
		os.MkdirAll(wd, 0755)
		tmp := filepath.Join(wd, "ingest.mp3")
		data, err := os.ReadFile(host)
		if err != nil {
			skipped++
			continue
		}
		os.WriteFile(tmp, data, 0644)
		if err := library.CasPromote(*generated, key, tmp, host); err != nil {
			log.Printf("ingest failed %s: %v", host, err)
			skipped++
			continue
		}
		stamped++
	}
	log.Printf("ingest: stamped=%d skipped=%d", stamped, skipped)
}
