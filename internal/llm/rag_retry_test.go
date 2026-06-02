package llm

import (
	"errors"
	"testing"
)

// fakeEmbedStore fails the first failN UpdateChunkEmbedding calls with err,
// then succeeds. Counts total attempts.
type fakeEmbedStore struct {
	failN    int
	err      error
	attempts int
}

func (f *fakeEmbedStore) UpdateChunkEmbedding(chunkID int64, embedding []byte) error {
	f.attempts++
	if f.attempts <= f.failN {
		return f.err
	}
	return nil
}

func TestSaveEmbeddingWithRetry(t *testing.T) {
	busy := errors.New("SQLITE_BUSY: database is locked")

	// Retries through transient BUSY, then succeeds.
	fs := &fakeEmbedStore{failN: 3, err: busy}
	if err := saveEmbeddingWithRetry(fs, 1, []byte("v")); err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if fs.attempts != 4 {
		t.Errorf("attempts = %d, want 4 (3 busy + 1 ok)", fs.attempts)
	}

	// Non-busy error fails fast (one attempt, no retry).
	other := errors.New("disk full")
	fs2 := &fakeEmbedStore{failN: 5, err: other}
	if err := saveEmbeddingWithRetry(fs2, 1, []byte("v")); err == nil {
		t.Fatal("expected non-busy error to surface")
	}
	if fs2.attempts != 1 {
		t.Errorf("attempts = %d, want 1 (non-busy must not retry)", fs2.attempts)
	}

	// Persistent BUSY exhausts the 5 attempts and returns the error.
	fs3 := &fakeEmbedStore{failN: 99, err: busy}
	if err := saveEmbeddingWithRetry(fs3, 1, []byte("v")); err == nil {
		t.Fatal("expected persistent busy to surface after retries")
	}
	if fs3.attempts != 5 {
		t.Errorf("attempts = %d, want 5 (capped)", fs3.attempts)
	}
}
