package board

import (
	"context"
	"database/sql"
	"net/url"
)

// dsnReadOnly is dsn without the writes.
//
// A replica serves a copy, and the copy must not be modified by the act of reading it: Open runs
// migrations, which rewrites schema and creates -wal/-shm files beside a snapshot that is supposed
// to be a frozen artifact. immutable=1 says the file will not change underneath SQLite (true here:
// the replica swaps the whole file and opens a new handle), so no locks are taken and no journal is
// written — which is also what makes opening it cheap.
func dsnReadOnly(path string) string {
	u := url.URL{
		Scheme:   "file",
		OmitHost: true,
		Path:     path,
		RawQuery: "mode=ro&immutable=1",
	}
	return u.String()
}

// OpenReadOnly opens a snapshot for serving. It does not migrate: a snapshot is whatever version it
// is, and a replica that "upgraded" it would be a writer — the one thing this process must never be.
func OpenReadOnly(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsnReadOnly(path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}
