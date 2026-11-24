package db

import (
	"path/filepath"
	"sync"
	"testing"
)

// Two processes starting together (a Kamal overlap, or `-migrate` beside the
// server) open the same fresh file. Without the migration lock one of them fails
// with SQLITE_BUSY on the first connection or the switch to WAL, neither of which
// busy_timeout covers, or both run a migration. Goroutines stand in for the
// processes: the lock is per file descriptor, so they exclude each other too.
func TestConcurrentOpensOfAFreshDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")
	const n = 8
	start := make(chan struct{})
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			d, err := Open(path)
			if err == nil {
				d.Close()
			}
			errs[i] = err
		}()
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("opener %d: %v", i, err)
		}
	}

	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var rows, distinct int
	if err := d.QueryRow(`SELECT count(*), count(DISTINCT version_id) FROM goose_db_version`).Scan(&rows, &distinct); err != nil {
		t.Fatal(err)
	}
	// Version 0 and migrations 1..10, each recorded once.
	if rows != distinct || rows != 11 {
		t.Errorf("goose_db_version has %d rows, %d distinct versions; want 11, each once", rows, distinct)
	}
}
