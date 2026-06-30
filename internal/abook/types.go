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
	Format   string `json:"format"`  // always "abook"
	Version  int    `json:"version"` // container format version (2)
	WorkID   int64  `json:"work_id"`
	Title    string `json:"title"`
	Author   string `json:"author"`
	Language string `json:"language"`
	// SourceKind summarizes what this work is: "aligned" | "transcript" |
	// "text-only" | "audio-only". Drives the library listing badge.
	SourceKind string `json:"source_kind"`
	// Version stamps mirrored from works (and into book.db.meta). SchemaVersion
	// is the book.db shape; ContentVersion is the RFC3339 UTC last-process time.
	SchemaVersion  int    `json:"schema_version"`
	ContentVersion string `json:"content_version"`
	Generator      string `json:"generator"`
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
	// Checksums maps in-zip asset path -> "sha256:<hex>". Currently book.db.
	Checksums map[string]string `json:"checksums"`
}

// Assets maps the logical assets to their paths inside the zip.
type Assets struct {
	DB       string `json:"db"`        // "book.db"
	AudioDir string `json:"audio_dir"` // "audio/"
	Cover    string `json:"cover"`     // "cover.jpg" ("" when absent)
}
