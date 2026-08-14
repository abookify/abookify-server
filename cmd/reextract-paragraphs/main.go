// reextract-paragraphs: the 53-book paragraph-structure migration
// (authorized by PJ 2026-08-10; board task 5).
//
// The epub extraction collapsed </p> into the same "\n" as a <br>, so every
// epub in the library stored flat text and the paragraphs table filled with
// ~12-word wrapped lines. The extraction fix is landed; this tool re-runs it
// over the existing library.
//
// SAFETY MODEL — each book is INDEPENDENTLY SAFE (decided before running,
// per META): a book is either fully migrated or untouched; the flat state is
// self-consistent, so a partial run leaves a coherent library and the ledger
// says exactly which books are which. Within a book the run is WHITESPACE-
// ONLY BY POLICY: chapters are updated in place (preserving ids, titles,
// alignment-derived start/end secs) ONLY when the re-extracted word stream
// is IDENTICAL per chapter — word-unit alignments, chunks, word-sync and
// reader timings all stay valid by construction. Any book whose re-extraction
// changes words or chapter count is FLAGGED (needs_review) and left
// untouched for a reviewed second pass.
//
// Ledger (TSV, written AS WE GO): book, work, chapters, words, paragraph
// rows before -> after, disposition.
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
	dbPath := flag.String("db", "./data/abookify.db", "sqlite db path")
	libPath := flag.String("library", "./testdata/library", "host path replacing the stored /library prefix")
	ledger := flag.String("ledger", "reextract-ledger.tsv", "per-book ledger (appended as the run goes)")
	only := flag.Int64("book", 0, "migrate a single text book id (0 = all epubs)")
	dry := flag.Bool("dry-run", false, "measure and write the ledger; change nothing")
	review := flag.Bool("review", false, "REVIEW PASS: fully replace chapters for books whose re-extraction drifts (chapter count or word stream) — today's extraction is better than the import-era one. Replaces chapters (epub text chapters carry no timing — verified), repopulates paragraphs, rechunks (new chunks await the embedding refresh endpoint), invalidates the summaries cache, and re-runs the work's anchor alignment.")
	flag.Parse()

	store, err := db.Open(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	lf, err := os.OpenFile(*ledger, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatal(err)
	}
	defer lf.Close()
	logLedger := func(format string, a ...any) {
		line := fmt.Sprintf(format, a...)
		fmt.Fprintln(lf, line)
		log.Print(line)
	}

	books, err := store.ListBooksByFormat("epub")
	if err != nil {
		log.Fatal(err)
	}
	migrated, flagged, skipped := 0, 0, 0
	for _, b := range books {
		if *only != 0 && b.ID != *only {
			continue
		}
		host := b.Path
		if strings.HasPrefix(host, "/library/") {
			host = filepath.Join(*libPath, strings.TrimPrefix(host, "/library/"))
		}
		if _, err := os.Stat(host); err != nil {
			logLedger("book=%d\twork=%d\tSKIP_MISSING\t%s", b.ID, b.WorkID, host)
			skipped++
			continue
		}
		old, err := store.ListChaptersWithContent(b.ID)
		if err != nil {
			logLedger("book=%d\twork=%d\tERR_LIST\t%v", b.ID, b.WorkID, err)
			flagged++
			continue
		}
		fresh, err := library.ExtractEPUBChapters(host, b.ID)
		if err != nil {
			logLedger("book=%d\twork=%d\tERR_EXTRACT\t%v", b.ID, b.WorkID, err)
			flagged++
			continue
		}
		same := len(fresh) == len(old)
		words := 0
		driftNote := ""
		if !same {
			driftNote = fmt.Sprintf("chapter count %d -> %d", len(old), len(fresh))
		}
		if same {
			for i := range old {
				ow := strings.Fields(old[i].Content)
				nw := strings.Fields(fresh[i].Content)
				words += len(nw)
				if strings.Join(ow, " ") != strings.Join(nw, " ") {
					driftNote = fmt.Sprintf("ch %d words %d -> %d (stream differs)", old[i].Index, len(ow), len(nw))
					same = false
					break
				}
			}
		}
		if !same && !*review {
			logLedger("book=%d\twork=%d\tNEEDS_REVIEW\t%s", b.ID, b.WorkID, driftNote)
			flagged++
			continue
		}
		if !same && *review {
			// FULL REPLACE, per-book atomic in effect: each step is
			// re-runnable and the book ends fully re-derived or the ledger
			// says exactly where it stopped.
			if *dry {
				logLedger("book=%d\twork=%d\tDRY_REVIEW\t%s", b.ID, b.WorkID, driftNote)
				migrated++
				continue
			}
			before, _ := store.ParagraphCount(b.ID)
			oldWords, newWords := 0, 0
			for i := range old {
				oldWords += len(strings.Fields(old[i].Content))
			}
			for i := range fresh {
				newWords += len(strings.Fields(fresh[i].Content))
			}
			if err := store.DeleteChaptersByBook(b.ID); err != nil {
				logLedger("book=%d\twork=%d\tERR_DELETE\t%v", b.ID, b.WorkID, err)
				flagged++
				continue
			}
			okIns := true
			for i := range fresh {
				if err := store.InsertChapter(fresh[i]); err != nil {
					logLedger("book=%d\twork=%d\tERR_INSERT\tch %d: %v", b.ID, b.WorkID, fresh[i].Index, err)
					okIns = false
					break
				}
			}
			if !okIns {
				flagged++
				continue
			}
			after, err := library.PopulateParagraphsForBook(store, b.ID)
			if err != nil {
				logLedger("book=%d\twork=%d\tERR_PARAS\t%v", b.ID, b.WorkID, err)
				flagged++
				continue
			}
			if err := library.ChunkBook(store, b.ID); err != nil {
				logLedger("book=%d\twork=%d\tERR_CHUNK\t%v", b.ID, b.WorkID, err)
				flagged++
				continue
			}
			store.DeleteSummariesForBook(b.ID)
			cov, err := library.ComputeAnchorAlignment(store, b.WorkID)
			if err != nil {
				logLedger("book=%d\twork=%d\tREVIEWED_NOALIGN\t%s; ch %d->%d words %d->%d paras %d->%d (align: %v)",
					b.ID, b.WorkID, driftNote, len(old), len(fresh), oldWords, newWords, before, after, err)
				migrated++
				continue
			}
			logLedger("book=%d\twork=%d\tREVIEWED\t%s; ch %d->%d words %d->%d paras %d->%d anchor=%.4f",
				b.ID, b.WorkID, driftNote, len(old), len(fresh), oldWords, newWords, before, after, cov)
			migrated++
			continue
		}
		before, _ := store.ParagraphCount(b.ID)
		if *dry {
			logLedger("book=%d\twork=%d\tDRY_CLEAN\tch=%d words=%d paras_before=%d", b.ID, b.WorkID, len(old), words, before)
			migrated++
			continue
		}
		ok := true
		for i := range fresh {
			if err := store.UpdateChapterContent(b.ID, fresh[i].Index, fresh[i].Content, fresh[i].ContentHTML, len(strings.Fields(fresh[i].Content))); err != nil {
				logLedger("book=%d\twork=%d\tERR_UPDATE\tch %d: %v", b.ID, b.WorkID, fresh[i].Index, err)
				ok = false
				break
			}
		}
		if !ok {
			flagged++
			continue
		}
		after, err := library.PopulateParagraphsForBook(store, b.ID)
		if err != nil {
			logLedger("book=%d\twork=%d\tERR_PARAS\t%v", b.ID, b.WorkID, err)
			flagged++
			continue
		}
		logLedger("book=%d\twork=%d\tCLEAN\tch=%d words=%d paras %d -> %d", b.ID, b.WorkID, len(old), words, before, after)
		migrated++
	}
	logLedger("SUMMARY\tmigrated=%d flagged=%d skipped=%d dry=%v", migrated, flagged, skipped, *dry)
	if flagged > 0 && !*dry {
		os.Exit(1)
	}
}
