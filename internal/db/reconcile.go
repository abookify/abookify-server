package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Store reconciliation — REPORTER, NOT A CLEANER (board 18, META 2026-08-15).
//
// The library lives in two stores that nothing cross-checks: the SQLite rows
// and the files they point at. They drift in BOTH directions and each drift
// was found by accident, hours into unrelated work:
//
//   - rows without files: a deleted edition's playback position survived (and
//     kept accepting writes) — the phantom-position bug;
//   - files without rows: three /generated edition dirs with zero book rows
//     (one empty, one 2.2 MB, one 6.5 MB) sat unreachable for weeks.
//
// This is the instrument that would have said so on day one. It runs one
// read-only sweep — SELECTs plus os.Stat/ReadDir, never a write — and reports:
//
//   rows_without_files   book rows whose file is gone (root-aware: an
//                        unreachable/partial root is ONE finding, not N deletes)
//   rows_without_rows    references whose target row no longer exists (the
//                        classes CleanupOrphanedRows does NOT sweep — positions,
//                        bookmarks, conditions, provenance, summaries, …)
//   files_without_rows   media under the library roots no row indexes; edition
//                        dirs / chapter files under <generated> no row serves
//   derived              caches and sidecars that outlive their owner (waveform
//                        cache, CAS objects, in-flight work dirs) — disk, not
//                        correctness
//
// It fixes nothing. Deleting on inference is exactly the fault class the
// August purge reconciliation was about; a person reads this and decides.
// Third member of the instrument family (selfdesc-audit: data misdescribing
// itself; config-blindspots: tests that cannot reach; THIS: stores disagreeing
// with each other). Runnable on demand from `cmd/store-reconcile` or wrapped
// by an HTTP handler via (*Store).Reconcile.

type ReconcileOptions struct {
	// GeneratedDir is the server's generated-audio dir (TTS editions, CAS,
	// waveform cache). Empty skips the generated-side checks.
	GeneratedDir string
	// LibraryRoots overrides the library_roots table (tests, or a host that
	// maps the container's /library elsewhere). Empty = read the table.
	LibraryRoots []string
	// MaxExamples caps the sample paths/ids per finding (default 10).
	MaxExamples int
	// SuspectMissingFraction: above this share of missing files under ONE
	// reachable root the root is reported as partial/unmounted instead of
	// listing every book (default 0.5). Mirrors ReconcileLibraryRoots' guard.
	SuspectMissingFraction float64
}

type ReconcileFinding struct {
	Severity  string   `json:"severity"`  // HIGH | MED | LOW | INFO
	Direction string   `json:"direction"` // rows_without_files | rows_without_rows | files_without_rows | derived
	Class     string   `json:"class"`     // stable slug, one per failure shape
	Count     int      `json:"count"`
	Bytes     int64    `json:"bytes,omitempty"`
	Detail    string   `json:"detail"`
	Examples  []string `json:"examples,omitempty"`
}

type ReconcileRoot struct {
	Path      string `json:"path"`
	Exists    bool   `json:"exists"`
	Books     int    `json:"books"`
	Missing   int    `json:"missing"`
	Suspected bool   `json:"suspected_unmounted"`
}

type ReconcileReport struct {
	CheckedAt    time.Time          `json:"checked_at"`
	GeneratedDir string             `json:"generated_dir,omitempty"`
	Roots        []ReconcileRoot    `json:"roots"`
	Findings     []ReconcileFinding `json:"findings"`
	Totals       map[string]int     `json:"totals"` // findings' Count summed per direction
	Clean        bool               `json:"clean"`  // no HIGH or MED finding
	Skipped      []string           `json:"skipped,omitempty"`
}

// Reconcile runs the sweep against this store.
func (s *Store) Reconcile(opts ReconcileOptions) (*ReconcileReport, error) {
	return ReconcileStores(s.db, opts)
}

// ReconcileStores runs the sweep against any open handle (the CLI opens the
// database read-only, which Open() cannot).
func ReconcileStores(sqlDB *sql.DB, opts ReconcileOptions) (*ReconcileReport, error) {
	if opts.MaxExamples <= 0 {
		opts.MaxExamples = 10
	}
	if opts.SuspectMissingFraction <= 0 {
		opts.SuspectMissingFraction = 0.5
	}
	r := &reconciler{db: sqlDB, opts: opts, rep: &ReconcileReport{
		CheckedAt: time.Now().UTC(), GeneratedDir: opts.GeneratedDir, Totals: map[string]int{},
	}}
	if err := r.loadBooks(); err != nil {
		return nil, err
	}
	r.rowsWithoutFiles()
	r.rowsWithoutRows()
	r.filesWithoutRows()
	r.finish()
	return r.rep, nil
}

type reconBook struct {
	ID     int64
	WorkID int64
	RootID int64
	Path   string
	Origin string
}

type reconciler struct {
	db    *sql.DB
	opts  ReconcileOptions
	rep   *ReconcileReport
	books []reconBook
	paths map[string]reconBook // every books.path (real and virtual)
}

func (r *reconciler) add(sev, dir, class, detail string, count int, bytes int64, examples []string) {
	if count == 0 {
		return
	}
	if len(examples) > r.opts.MaxExamples {
		examples = examples[:r.opts.MaxExamples]
	}
	r.rep.Findings = append(r.rep.Findings, ReconcileFinding{
		Severity: sev, Direction: dir, Class: class, Count: count, Bytes: bytes, Detail: detail, Examples: examples,
	})
}

func (r *reconciler) skip(what string, err error) {
	r.rep.Skipped = append(r.rep.Skipped, fmt.Sprintf("%s: %v", what, err))
}

func (r *reconciler) loadBooks() error {
	rows, err := r.db.Query(`SELECT id, work_id, COALESCE(root_id, 0), path, origin FROM books`)
	if err != nil {
		return fmt.Errorf("reconcile: list books: %w", err)
	}
	defer rows.Close()
	r.paths = map[string]reconBook{}
	for rows.Next() {
		var b reconBook
		if err := rows.Scan(&b.ID, &b.WorkID, &b.RootID, &b.Path, &b.Origin); err != nil {
			return err
		}
		r.books = append(r.books, b)
		r.paths[b.Path] = b
	}
	return rows.Err()
}

// ---------------------------------------------------------------- rows → files

func (r *reconciler) rowsWithoutFiles() {
	roots := r.opts.LibraryRoots
	if len(roots) == 0 {
		rows, err := r.db.Query(`SELECT path FROM library_roots ORDER BY position, id`)
		if err == nil {
			for rows.Next() {
				var p string
				if rows.Scan(&p) == nil {
					roots = append(roots, p)
				}
			}
			rows.Close()
		}
	}
	rootOf := func(p string) string {
		for _, root := range roots {
			if p == root || strings.HasPrefix(p, strings.TrimSuffix(root, "/")+"/") {
				return root
			}
		}
		return ""
	}
	type acc struct {
		books, missing int
		examples       []string
	}
	perRoot := map[string]*acc{}
	for _, root := range roots {
		perRoot[root] = &acc{}
	}
	var unrooted acc // root_id = 0 real files: TTS editions under <generated>, imports outside every root
	for _, b := range r.books {
		if strings.HasPrefix(b.Path, "generated://") {
			continue // virtual by design (STT transcripts) — no file expected
		}
		root := rootOf(b.Path)
		a := &unrooted
		if root != "" {
			a = perRoot[root]
		}
		a.books++
		if _, err := os.Stat(b.Path); err != nil {
			a.missing++
			a.examples = append(a.examples, fmt.Sprintf("book %d (work %d, %s) %s", b.ID, b.WorkID, b.Origin, b.Path))
		}
	}
	for _, root := range roots {
		a := perRoot[root]
		st, err := os.Stat(root)
		exists := err == nil && st.IsDir()
		rr := ReconcileRoot{Path: root, Exists: exists, Books: a.books, Missing: a.missing}
		if a.books > 0 && (!exists || float64(a.missing)/float64(a.books) > r.opts.SuspectMissingFraction) {
			rr.Suspected = true
			r.add("HIGH", "rows_without_files", "library_root_unreachable_or_partial",
				fmt.Sprintf("%s: %d of %d indexed files missing — reads as an unmounted or partial root, not %d deletions; mount it before acting on anything below", root, a.missing, a.books, a.missing),
				1, 0, a.examples)
		} else {
			r.add("HIGH", "rows_without_files", "book_file_missing",
				fmt.Sprintf("book rows under %s whose file no longer exists (the row still lists, streams 404)", root),
				a.missing, 0, a.examples)
		}
		r.rep.Roots = append(r.rep.Roots, rr)
	}
	r.add("HIGH", "rows_without_files", "book_file_missing",
		"book rows outside every library root (TTS editions, imports) whose file no longer exists — the phantom class: the row still lists, streams 404, and positions against it keep saving",
		unrooted.missing, 0, unrooted.examples)

	// TTS content-addressed store: a chapter's sidecar names the CAS object its
	// hardlink came from. A dangling key means "done" can no longer be proven
	// against the text, so the chapter re-synthesizes at the next generate.
	if r.opts.GeneratedDir != "" {
		var dangling, sidecarOnly []string
		filepath.WalkDir(r.opts.GeneratedDir, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".mp3.cas") {
				return nil
			}
			key, _ := os.ReadFile(p)
			k := strings.TrimSpace(string(key))
			if len(k) >= 2 {
				if _, err := os.Stat(filepath.Join(r.opts.GeneratedDir, "cas", k[:2], k+".mp3")); err != nil {
					dangling = append(dangling, p)
				}
			}
			if _, err := os.Stat(strings.TrimSuffix(p, ".cas")); err != nil {
				sidecarOnly = append(sidecarOnly, p)
			}
			return nil
		})
		r.add("MED", "rows_without_files", "tts_sidecar_dangling_cas_key",
			"chapter sidecars whose content key names no CAS object (audio still plays via its hardlink; the resume proof is gone, so the chapter re-synthesizes next generate)",
			len(dangling), 0, dangling)
		r.add("LOW", "derived", "tts_sidecar_without_chapter",
			"sidecars whose chapter mp3 is gone", len(sidecarOnly), 0, sidecarOnly)
	}
}

// ----------------------------------------------------------------- rows → rows

type refCheck struct {
	table, col, parent string
	where              string // extra predicate on the child table
	sev, note          string
}

func (r *reconciler) rowsWithoutRows() {
	// CleanupOrphanedRows (boot) sweeps chunks/paragraphs/chapters/alignments/
	// sync_data/chapter_links/bookmarks(work)/positions(work)/events/characters.
	// Everything here is EITHER outside that list OR keyed by book_id where the
	// sweep only checks work_id — the exact gap the phantom position lived in.
	checks := []refCheck{
		{"playback_positions", "book_id", "books", "", "HIGH", "a resume position for a book that no longer exists — the phantom-position bug: it lists a resume point and keeps accepting saves"},
		{"bookmarks", "book_id", "books", "", "MED", "bookmarks/highlights on a book that no longer exists"},
		{"book_conditions", "book_id", "books", "", "MED", "degraded/complete verdicts for books that no longer exist"},
		{"source_provenance", "ref_id", "books", "scope = 'book'", "MED", "provenance declarations for books that no longer exist (a cleared flag with no source behind it)"},
		{"source_provenance", "ref_id", "works", "scope = 'cover'", "MED", "cover provenance for works that no longer exist"},
		{"summaries", "book_id", "books", "", "LOW", "cached chapter summaries/recaps for books that no longer exist"},
		{"source_scans", "book_id", "books", "", "LOW", "source damage scans for books that no longer exist"},
		{"text_trust", "work_id", "works", "", "LOW", "text-trust verdicts for works that no longer exist"},
		{"timing_results", "work_id", "works", "", "LOW", "timing-audit results for works that no longer exist"},
		{"qa_sessions", "work_id", "works", "", "MED", "Q&A chats for works that no longer exist"},
		{"qa_messages", "session_id", "qa_sessions", "", "LOW", "Q&A messages whose chat is gone"},
		{"books", "work_id", "works", "", "HIGH", "books whose work no longer exists (unreachable from every list)"},
		{"books", "root_id", "library_roots", "root_id != 0", "MED", "books attributed to a library root that no longer exists"},
		{"sync_data", "audio_book_id", "books", "", "MED", "karaoke word timings for an audio book that no longer exists"},
		{"alignments", "from_book_id", "books", "", "MED", "alignments from a book that no longer exists"},
		{"alignments", "to_book_id", "books", "", "MED", "alignments to a book that no longer exists"},
		{"chapter_links", "audio_book_id", "books", "", "MED", "chapter links whose audio book no longer exists"},
		{"chapter_links", "text_book_id", "books", "", "MED", "chapter links whose text book no longer exists"},
		{"chapters", "book_id", "books", "", "MED", "chapters whose book no longer exists"},
		{"chunks", "book_id", "books", "", "MED", "Q&A chunks whose book no longer exists"},
		{"paragraphs", "book_id", "books", "", "LOW", "paragraphs whose book no longer exists"},
		{"characters", "book_id", "books", "", "LOW", "cast entries whose book no longer exists"},
		{"character_mentions", "character_id", "characters", "", "LOW", "cast mentions whose character no longer exists"},
		{"playback_events", "work_id", "works", "", "LOW", "listening events for works that no longer exist"},
	}
	for _, c := range checks {
		if !r.hasColumn(c.table, c.col) {
			continue
		}
		where := fmt.Sprintf("%s > 0 AND %s NOT IN (SELECT id FROM %s)", c.col, c.col, c.parent)
		if c.where != "" {
			where += " AND " + c.where
		}
		n, ex, err := r.countAndSample(c.table, c.col, where)
		if err != nil {
			r.skip(c.table+"."+c.col, err)
			continue
		}
		r.add(c.sev, "rows_without_rows", c.table+"."+c.col+"_dangling", c.note, n, 0, ex)
	}
	// A reference of 0 is "unset", not dangling — but on tables where the
	// column is meant to identify a real book it still points nowhere.
	for _, c := range []refCheck{
		{"playback_positions", "book_id", "", "", "LOW", "resume positions with book_id = 0 (unset): the position cannot name the file it belongs to"},
		{"bookmarks", "book_id", "", "", "LOW", "bookmarks with book_id = 0 (unset)"},
	} {
		if !r.hasColumn(c.table, c.col) {
			continue
		}
		n, ex, err := r.countAndSample(c.table, "work_id", c.col+" = 0")
		if err != nil {
			r.skip(c.table+"."+c.col+"=0", err)
			continue
		}
		r.add(c.sev, "rows_without_rows", c.table+"."+c.col+"_unset", c.note, n, 0, prefixEach("work ", ex))
	}
	// Cross-work: a position/bookmark that names a book belonging to a
	// different work than the row says (a merge/split left it behind).
	for _, t := range []string{"playback_positions", "bookmarks"} {
		if !r.hasColumn(t, "book_id") {
			continue
		}
		q := fmt.Sprintf(`SELECT t.work_id, t.book_id, b.work_id FROM %s t JOIN books b ON b.id = t.book_id WHERE b.work_id != t.work_id`, t)
		rows, err := r.db.Query(q)
		if err != nil {
			r.skip(t+" cross-work", err)
			continue
		}
		var n int
		var ex []string
		for rows.Next() {
			var w, b, bw int64
			if rows.Scan(&w, &b, &bw) == nil {
				n++
				ex = append(ex, fmt.Sprintf("row work %d → book %d (which belongs to work %d)", w, b, bw))
			}
		}
		rows.Close()
		r.add("MED", "rows_without_rows", t+"_cross_work",
			"rows whose book belongs to a different work than the row claims (left behind by a merge/split)", n, 0, ex)
	}
}

func (r *reconciler) hasColumn(table, col string) bool {
	rows, err := r.db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt sql.NullString
		if rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk) == nil && name == col {
			return true
		}
	}
	return false
}

func (r *reconciler) countAndSample(table, col, where string) (int, []string, error) {
	var n int
	if err := r.db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE %s`, table, where)).Scan(&n); err != nil {
		return 0, nil, err
	}
	if n == 0 {
		return 0, nil, nil
	}
	rows, err := r.db.Query(fmt.Sprintf(`SELECT DISTINCT %s FROM %s WHERE %s ORDER BY 1 LIMIT %d`, col, table, where, r.opts.MaxExamples))
	if err != nil {
		return n, nil, err
	}
	defer rows.Close()
	var ex []string
	for rows.Next() {
		var v int64
		if rows.Scan(&v) == nil {
			ex = append(ex, strconv.FormatInt(v, 10))
		}
	}
	return n, ex, nil
}

func prefixEach(p string, in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = p + s
	}
	return out
}

// ---------------------------------------------------------------- files → rows

var reconMediaExt = map[string]bool{
	".epub": true, ".mobi": true, ".azw3": true, ".azw": true, ".pdf": true, ".txt": true, ".text": true,
	".mp3": true, ".m4b": true, ".m4a": true, ".flac": true, ".aac": true, ".opus": true, ".ogg": true,
}

func (r *reconciler) filesWithoutRows() {
	for _, root := range r.rep.Roots {
		if !root.Exists || root.Suspected {
			continue
		}
		r.sweepLibraryRoot(root.Path)
	}
	if r.opts.GeneratedDir != "" {
		r.sweepGenerated(r.opts.GeneratedDir)
	}
}

func (r *reconciler) sweepLibraryRoot(root string) {
	type bucket struct {
		n     int
		bytes int64
		ex    []string
	}
	unindexed, previews, exports, unpacked, staging := &bucket{}, &bucket{}, &bucket{}, &bucket{}, &bucket{}
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if p != root && strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(name))
		rel, _ := filepath.Rel(root, p)
		top := strings.Split(rel, string(filepath.Separator))[0]
		b := unindexed
		switch {
		case ext == ".abook":
			b = exports
		case top == "tts-previews":
			b = previews
		case top == "abooks":
			// The scanner skips <root>/abooks by design (unpacked .abook
			// imports live here), so media inside is never indexed — which
			// is fine while it is tiny and a surprise at ten gigabytes.
			if !reconMediaExt[ext] {
				return nil
			}
			b = unpacked
		case top == "incoming" || top == "processing" || top == "failed":
			b = staging
		case !reconMediaExt[ext]:
			return nil
		}
		if _, ok := r.paths[p]; ok {
			return nil
		}
		info, _ := d.Info()
		b.n++
		if info != nil {
			b.bytes += info.Size()
		}
		b.ex = append(b.ex, p)
		return nil
	})
	r.add("MED", "files_without_rows", "library_media_unindexed",
		fmt.Sprintf("media files under %s that no book row indexes (the scanner never picked them up, or their rows were deleted): invisible in the library", root),
		unindexed.n, unindexed.bytes, unindexed.ex)
	r.add("INFO", "files_without_rows", "library_voice_previews",
		"voice preview clips under tts-previews/ (never indexed by design)", previews.n, previews.bytes, nil)
	r.add("INFO", "files_without_rows", "library_abook_archives",
		".abook archives sitting in the library (exports/imports; never indexed as books by design)", exports.n, exports.bytes, exports.ex)
	r.add("LOW", "files_without_rows", "library_abooks_unpacked_media",
		"media unpacked under <root>/abooks/ that no book row indexes — the scanner skips this dir by design, so whatever import or fixture left it is invisible AND still on disk",
		unpacked.n, unpacked.bytes, unpacked.ex)
	r.add("LOW", "files_without_rows", "library_ingest_staging",
		"files in incoming/processing/failed — an ingest that never finished", staging.n, staging.bytes, staging.ex)
}

func (r *reconciler) sweepGenerated(gen string) {
	entries, err := os.ReadDir(gen)
	if err != nil {
		r.skip("generated dir", err)
		return
	}
	// Live CAS keys = every sidecar under the edition dirs.
	liveKeys := map[string]bool{}
	bookIDs := map[int64]bool{}
	for _, b := range r.books {
		bookIDs[b.ID] = true
	}
	var orphanDirs, partialFiles, unknown, workDirs []string
	var orphanDirBytes, partialBytes, casBytes, wfBytes int64
	var partialN, casN, wfN int
	var casUnref []string
	var wfStale []string
	for _, e := range entries {
		p := filepath.Join(gen, e.Name())
		switch {
		case e.IsDir() && strings.HasPrefix(e.Name(), "tts-book-"):
			var referenced, unreferenced int
			var unrefBytes int64
			var unrefEx []string
			filepath.WalkDir(p, func(fp string, d os.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return nil
				}
				if strings.HasSuffix(fp, ".mp3.cas") {
					if k, err := os.ReadFile(fp); err == nil {
						liveKeys[strings.TrimSpace(string(k))] = true
					}
					return nil
				}
				if strings.ToLower(filepath.Ext(fp)) != ".mp3" {
					return nil
				}
				if _, ok := r.paths[fp]; ok {
					referenced++
					return nil
				}
				unreferenced++
				if info, _ := d.Info(); info != nil {
					unrefBytes += info.Size()
				}
				unrefEx = append(unrefEx, fp)
				return nil
			})
			switch {
			case referenced == 0 && unreferenced > 0:
				orphanDirs = append(orphanDirs, fmt.Sprintf("%s (%d files, %s)", p, unreferenced, humanBytes(unrefBytes)))
				orphanDirBytes += unrefBytes
			case referenced == 0 && unreferenced == 0:
				orphanDirs = append(orphanDirs, p+" (empty)")
			case unreferenced > 0:
				partialN += unreferenced
				partialBytes += unrefBytes
				partialFiles = append(partialFiles, unrefEx...)
			}
		case e.IsDir() && e.Name() == "cas":
			// walked below, after every edition sidecar has been read
		case e.IsDir() && e.Name() == "waveforms":
			files, _ := os.ReadDir(p)
			for _, f := range files {
				idStr := strings.TrimSuffix(f.Name(), filepath.Ext(f.Name()))
				id, err := strconv.ParseInt(idStr, 10, 64)
				if err != nil || bookIDs[id] {
					continue
				}
				wfN++
				if info, _ := f.Info(); info != nil {
					wfBytes += info.Size()
				}
				wfStale = append(wfStale, filepath.Join(p, f.Name()))
			}
		case e.IsDir() && e.Name() == "work":
			jobs, _ := os.ReadDir(p)
			for _, j := range jobs {
				info, _ := j.Info()
				age := ""
				if info != nil {
					age = " (" + time.Since(info.ModTime()).Round(time.Hour).String() + " old)"
				}
				workDirs = append(workDirs, filepath.Join(p, j.Name())+age)
			}
		default:
			unknown = append(unknown, p)
		}
	}
	// ReadDir is name-sorted ("cas" < "tts-book-…"), so the CAS pass runs here,
	// once every edition sidecar has contributed its live key.
	if casDir := filepath.Join(gen, "cas"); dirExists(casDir) {
		filepath.WalkDir(casDir, func(fp string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(fp, ".mp3") {
				return nil
			}
			key := strings.TrimSuffix(filepath.Base(fp), ".mp3")
			if !liveKeys[key] {
				casN++
				if info, _ := d.Info(); info != nil {
					casBytes += info.Size()
				}
				casUnref = append(casUnref, fp)
			}
			return nil
		})
	}
	sort.Strings(orphanDirs)
	r.add("MED", "files_without_rows", "tts_edition_dir_unreferenced",
		"generated edition dirs with ZERO book rows pointing into them — the three-orphan-dirs class: unreachable by construction, invisible in every list, paid for on disk",
		len(orphanDirs), orphanDirBytes, orphanDirs)
	r.add("MED", "files_without_rows", "tts_chapter_file_unreferenced",
		"chapter files inside a live edition dir that no book row serves (a chapter row deleted, or a file left by an older run)",
		partialN, partialBytes, partialFiles)
	r.add("LOW", "derived", "cas_object_unreferenced",
		"content-addressed audio objects no sidecar names any more (superseded narrations; safe to reclaim only once you are sure no generate is mid-flight)",
		casN, casBytes, casUnref)
	r.add("LOW", "derived", "waveform_cache_stale",
		"waveform cache entries keyed to book ids that no longer exist", wfN, wfBytes, wfStale)
	r.add("INFO", "derived", "tts_work_in_progress",
		"in-flight chapter assembly dirs under work/ (normal during a generate; stale if old and nothing is running)", len(workDirs), 0, workDirs)
	r.add("LOW", "files_without_rows", "generated_unknown_entry",
		"entries under the generated dir this reporter does not recognise", len(unknown), 0, unknown)
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func humanBytes(n int64) string {
	const k = 1024
	switch {
	case n >= k*k*k:
		return fmt.Sprintf("%.1f GB", float64(n)/(k*k*k))
	case n >= k*k:
		return fmt.Sprintf("%.1f MB", float64(n)/(k*k))
	case n >= k:
		return fmt.Sprintf("%.0f KB", float64(n)/k)
	}
	return fmt.Sprintf("%d B", n)
}

func (r *reconciler) finish() {
	rank := map[string]int{"HIGH": 0, "MED": 1, "LOW": 2, "INFO": 3}
	dirRank := map[string]int{"rows_without_files": 0, "rows_without_rows": 1, "files_without_rows": 2, "derived": 3}
	sort.SliceStable(r.rep.Findings, func(i, j int) bool {
		a, b := r.rep.Findings[i], r.rep.Findings[j]
		if dirRank[a.Direction] != dirRank[b.Direction] {
			return dirRank[a.Direction] < dirRank[b.Direction]
		}
		return rank[a.Severity] < rank[b.Severity]
	})
	r.rep.Clean = true
	for _, f := range r.rep.Findings {
		r.rep.Totals[f.Direction] += f.Count
		if f.Severity == "HIGH" || f.Severity == "MED" {
			r.rep.Clean = false
		}
	}
}

// FormatReconcileReport renders the report as the text the CLI prints.
func FormatReconcileReport(rep *ReconcileReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "store reconciliation — %s (reporter only; nothing was changed)\n", rep.CheckedAt.Format(time.RFC3339))
	for _, rt := range rep.Roots {
		state := "reachable"
		if !rt.Exists {
			state = "MISSING"
		} else if rt.Suspected {
			state = "SUSPECT (partial/unmounted)"
		}
		fmt.Fprintf(&b, "root %s: %s, %d indexed files, %d missing\n", rt.Path, state, rt.Books, rt.Missing)
	}
	if rep.GeneratedDir != "" {
		fmt.Fprintf(&b, "generated: %s\n", rep.GeneratedDir)
	}
	if len(rep.Findings) == 0 {
		b.WriteString("\nno drift: every row has its file, every file has its row.\n")
	}
	lastDir := ""
	for _, f := range rep.Findings {
		if f.Direction != lastDir {
			fmt.Fprintf(&b, "\n== %s\n", strings.ReplaceAll(f.Direction, "_", " "))
			lastDir = f.Direction
		}
		size := ""
		if f.Bytes > 0 {
			size = ", " + humanBytes(f.Bytes)
		}
		fmt.Fprintf(&b, "[%-4s] %s: %d%s\n       %s\n", f.Severity, f.Class, f.Count, size, f.Detail)
		for _, e := range f.Examples {
			fmt.Fprintf(&b, "       - %s\n", e)
		}
		if f.Count > len(f.Examples) && len(f.Examples) > 0 {
			fmt.Fprintf(&b, "       … %d more\n", f.Count-len(f.Examples))
		}
	}
	for _, s := range rep.Skipped {
		fmt.Fprintf(&b, "\nskipped: %s\n", s)
	}
	verdict := "CLEAN (no HIGH/MED)"
	if !rep.Clean {
		verdict = "DRIFT (HIGH/MED findings above)"
	}
	fmt.Fprintf(&b, "\n%s — rows_without_files=%d rows_without_rows=%d files_without_rows=%d derived=%d\n",
		verdict, rep.Totals["rows_without_files"], rep.Totals["rows_without_rows"], rep.Totals["files_without_rows"], rep.Totals["derived"])
	return b.String()
}
