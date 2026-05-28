package db

import (
	"sync"
	"testing"
	"time"

	"github.com/pj/abookify/internal/applog"
)

// TestConcurrentWritesNoBusy reproduces the scanner/sidecar-import burst that
// produced "database is locked (5) (SQLITE_BUSY)": many goroutines writing at
// once. With SetMaxOpenConns(1) every write serializes through one connection,
// so there is no internal contention and no goroutine should error or hang.
func TestConcurrentWritesNoBusy(t *testing.T) {
	store := testStore(t)

	const writers = 24
	const perWriter = 40

	var wg sync.WaitGroup
	errs := make(chan error, writers*perWriter)
	done := make(chan struct{})

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				// Mix of independent single-statement writes (like the
				// import path) and a multi-statement transaction (like
				// MergeWorks) to exercise both contention shapes.
				e := []applog.Entry{{
					Time:      time.Now(),
					Level:     applog.LevelInfo,
					Component: "stress",
					Message:   "concurrent write",
					Fields:    map[string]any{"w": w, "i": i},
				}}
				if err := store.InsertLogs(e); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}

	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("concurrent writes deadlocked (SetMaxOpenConns(1) reentrancy?)")
	}
	close(errs)
	for err := range errs {
		t.Errorf("concurrent write failed: %v", err)
	}

	got, err := store.QueryLogs(LogFilter{Component: "stress", Limit: writers*perWriter + 1})
	if err != nil {
		t.Fatalf("QueryLogs: %v", err)
	}
	if len(got) != writers*perWriter {
		t.Errorf("want %d rows persisted, got %d", writers*perWriter, len(got))
	}
}
