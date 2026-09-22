package server

import "github.com/pj/abookify/internal/diskfree"

// fsFreeBytes is kept for the existing callers; the implementation lives in
// internal/diskfree so the generator and importer can ask the same question.
func fsFreeBytes(path string) int64 { return diskfree.Free(path) }
