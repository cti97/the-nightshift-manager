package board

import (
	"database/sql"
	"testing"
)

// OpenForTest opens a board on a fixed clock. It lives in a non-_test file so
// the agent package can build the same fixture without duplicating schema
// knowledge. It is the only exported test helper in this package.
func OpenForTest(t *testing.T, path string) (*sql.DB, *FixedClock) {
	t.Helper()
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open board: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, NewFixedClock(1_700_000_000)
}
