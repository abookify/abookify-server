package abook

// BookDBSchemaVersion is the current shape of the per-work book.db carved
// into a .abook v2 container. Bump this whenever the book.db tables/columns
// change; mobile compares its installed copy's stamp against the server's
// (via GET /api/works/{id}/version) to decide whether to re-pull. This is
// independent of the manifest's container Version (which stays 2).
const BookDBSchemaVersion = 1

// Manifest is manifest.json — the lightweight identity + version + asset map
// at the root of a .abook v2 container. The heavy per-work detail lives in
// book.db; this file is what mobile reads first to decide install/update.
type Manifest struct {
	Format  string `json:"format"`  // always "abook"
	Version int    `json:"version"` // container format MAJOR version (2)
	// MinorVersion is the container format minor version. Additive features bump
	// it (readers keying off major Version==2 stay compatible). Minor 1 added
	// bundled original ebook source files under originals/.
	MinorVersion int    `json:"minor_version,omitempty"`
	WorkID       int64  `json:"work_id"`
	Title        string `json:"title"`
	Author       string `json:"author"`
	Language     string `json:"language"`
	// SourceKind summarizes what this work is: "aligned" | "transcript" |
	// "text-only" | "audio-only". Drives the library listing badge.
	SourceKind string `json:"source_kind"`
	// Version stamps mirrored from works (and into book.db.meta). SchemaVersion
	// is the book.db shape; ContentVersion is the RFC3339 UTC last-process time
	// (bumped on reprocess / re-align / re-TTS), so a consumer can tell which
	// generation this is. GeneratedAt is when THIS .abook was exported.
	SchemaVersion  int    `json:"schema_version"`
	ContentVersion string `json:"content_version"`
	GeneratedAt    string `json:"generated_at,omitempty"`
	// Provenance is a human-readable one-liner describing how this generation was
	// produced (audio source · alignment · text source).
	Provenance string `json:"provenance,omitempty"`
	Generator  string `json:"generator"`
	// Alignment summary — null when the work has no alignment.
	CoveragePct *float64 `json:"coverage_pct"`
	AlignMethod *string  `json:"align_method"`
	AlignUnit   *string  `json:"align_unit"`
	Assets      Assets   `json:"assets"`
	// HasEmbeddings advertises whether book.db's chunks carry embedding vectors,
	// so a consumer can decide to attempt on-device cosine search without
	// scanning the table. Additive (older readers ignore it); absent/false means
	// keyword-only retrieval.
	HasEmbeddings bool `json:"has_embeddings,omitempty"`
	// EmbeddingModel + EmbeddingDim identify which model produced the stored
	// vectors, so a consumer embeds the QUERY with a matching model (otherwise
	// cosine is meaningless — vectors from different models/dims aren't
	// comparable). EmbeddingDim is authoritative (bytes/4 of a stored blob);
	// EmbeddingModel is the matching model name. Empty when no embeddings.
	EmbeddingModel string `json:"embedding_model,omitempty"`
	EmbeddingDim   int    `json:"embedding_dim,omitempty"`
	// HasAudio / HasOriginalEbook make the container's contents explicit (vs
	// inferring from the file list). Audio is opt-in (size); the original ebook
	// source file(s) bundle by default (small, for fidelity + portability).
	HasAudio         bool `json:"has_audio"`
	HasOriginalEbook bool `json:"has_original_ebook"`
	// Originals lists the bundled original source files under originals/. The
	// carved book.db remains the render source; these are the untouched inputs.
	Originals []OriginalFile `json:"originals,omitempty"`
	// Publishing is present ONLY on a PUBLIC export (ExportOptions.Public): the
	// per-source declarations + clearances the exporter verified, and what it
	// decided about the cover. A file WITHOUT this block is not cleared for
	// public redistribution, whatever its attribution text says — the publish
	// gate (testing/provenance, bin/publish-check) refuses it.
	Publishing *Publishing `json:"publishing,omitempty"`
	// Checksums maps in-zip asset path -> "sha256:<hex>". Currently book.db.
	Checksums map[string]string `json:"checksums"`
}

// OriginalFile is one bundled original source file (an untouched ebook input).
type OriginalFile struct {
	Path     string `json:"path"`     // in-zip path, e.g. "originals/frankenstein.epub"
	Filename string `json:"filename"` // original filename
	Format   string `json:"format"`   // epub | mobi | azw3 | azw | txt | pdf
	Origin   string `json:"origin"`   // publisher_epub, user_upload, …
}

// Assets maps the logical assets to their paths inside the zip.
type Assets struct {
	DB           string `json:"db"`            // "book.db"
	AudioDir     string `json:"audio_dir"`     // "audio/"
	OriginalsDir string `json:"originals_dir"` // "originals/"
	Cover        string `json:"cover"`         // "cover.jpg" ("" when absent)
}

// Publishing records what a PUBLIC export verified. Cleared is a human's
// verified clearance (source_provenance.cleared + cleared_by), never a
// declaration alone — Owl Creek (2026-09-18) had a declaration that said
// "verify before public redistribution" and was published anyway.
type Publishing struct {
	Public     bool              `json:"public"`
	VerifiedAt string            `json:"verified_at"`
	Sources    []PublishedSource `json:"sources"`
	Cover      PublishedCover    `json:"cover"`
}

type PublishedSource struct {
	BookID    int64  `json:"book_id"`
	Media     string `json:"media"` // audio | text
	Format    string `json:"format"`
	Origin    string `json:"origin"`
	Kind      string `json:"kind"` // gutenberg | librivox | kokoro | pg-open-audiobook | ...
	SourceURL string `json:"source_url"`
	License   string `json:"license"`
	Cleared   bool   `json:"cleared"`
	ClearedBy string `json:"cleared_by"`
	Note      string `json:"note,omitempty"`
}

type PublishedCover struct {
	Bundled   bool   `json:"bundled"`
	Source    string `json:"source,omitempty"` // epub | audio | openlibrary | upload | abook | unknown
	Ref       string `json:"ref,omitempty"`
	Cleared   bool   `json:"cleared"`
	ClearedBy string `json:"cleared_by,omitempty"`
	Reason    string `json:"reason,omitempty"` // why it was or was not bundled
}
