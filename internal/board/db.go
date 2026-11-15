package board

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

const schemaVersion = 1

// SchemaVersion is exported for tests and for the migrate command.
func SchemaVersion() int { return schemaVersion }

// A migration upgrades a database one version, to Version.
type migration struct {
	version int
	script  string
}

// migrations lists every schema migration in ascending order. Entry i
// upgrades a database whose user_version is i to version i+1, so entry 0
// creates the schema in an empty database. Adding a migration means
// appending an entry (script + version) and bumping schemaVersion — not
// touching migrate.
var migrations = []migration{
	{version: schemaVersion, script: schema},
}

// Open opens (creating if needed) the board database, applies pending
// migrations, and returns a pool capped at ONE connection.
//
// The single connection is deliberate: it serialises writers so the claim
// transaction (§8 of the spec) needs no clever locking, and it makes
// "who changed this row" answerable. Raise it only with a claim redesign.
func Open(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := migrate(ctx, db); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return db, nil
}

// dsn builds the SQLite URI for path. url.URL percent-encodes the filename,
// so a '?' or '#' inside a path cannot be read as the start of the query;
// SQLite decodes %XX again on its side. For plain paths the output is
// byte-identical to "file:" + path + "?" + query.
func dsn(path string) string {
	u := url.URL{
		Scheme:   "file",
		OmitHost: true,
		Path:     path,
		RawQuery: "_pragma=busy_timeout(5000)" +
			"&_pragma=journal_mode(WAL)" +
			"&_pragma=foreign_keys(ON)" +
			"&_pragma=synchronous(NORMAL)",
	}
	return u.String()
}

func migrate(ctx context.Context, db *sql.DB) error {
	var v int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	if v > schemaVersion {
		return fmt.Errorf("database schema v%d is newer than this binary (v%d)", v, schemaVersion)
	}
	for i, m := range migrations {
		if m.version != i+1 {
			return fmt.Errorf("migrations are not contiguous: entry %d declares version %d, want %d", i+1, m.version, i+1)
		}
		if v >= m.version {
			continue
		}
		if err := applyMigration(ctx, db, m); err != nil {
			return err
		}
		v = m.version
	}
	return nil
}

// applyMigration runs one migration script inside a single transaction and
// stamps user_version with the migration's version.
func applyMigration(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmts, err := splitStatements(m.script)
	if err != nil {
		return fmt.Errorf("migration %d: %w", m.version, err)
	}
	for _, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migration %d: %w", m.version, err)
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
		return fmt.Errorf("migration %d: set user_version: %w", m.version, err)
	}
	return tx.Commit()
}

var (
	errLiteralSemicolon    = errors.New("contains ';' inside a quoted string literal")
	errUnterminatedLiteral = errors.New("has an unterminated string literal")
)

// splitStatements splits a migration script into its statements, skipping
// the blank padding between them. A ';' terminates a statement only outside
// '--' line comments: comments may contain ';' (the schema header does), and
// a ';' inside a quoted literal is rejected, because scripts are applied
// verbatim and a mangled split must fail loudly instead of executing half a
// statement. Trigger bodies (BEGIN ... END with ';' inside) cannot be
// expressed with this rule; the literal rule catches the common accidents.
func splitStatements(script string) ([]string, error) {
	stmts := []string(nil)
	var buf strings.Builder
	stmtNo := 0 // statements finished so far
	inComment := false
	inLiteral := false

	flush := func() {
		s := strings.TrimSpace(buf.String())
		buf.Reset()
		if s != "" {
			stmts = append(stmts, s)
		}
	}

	for i := 0; i < len(script); i++ {
		c := script[i]
		switch {
		case inComment:
			if c == '\n' {
				inComment = false
			}
			buf.WriteByte(c)
		case inLiteral:
			switch {
			case c == ';':
				return nil, fmt.Errorf("statement %d: %w", stmtNo+1, errLiteralSemicolon)
			case c == '\'' && i+1 < len(script) && script[i+1] == '\'':
				buf.WriteString("''")
				i++
			case c == '\'':
				inLiteral = false
				buf.WriteByte(c)
			default:
				buf.WriteByte(c)
			}
		case c == '-' && i+1 < len(script) && script[i+1] == '-':
			inComment = true
			buf.WriteString("--")
			i++
		case c == '\'':
			inLiteral = true
			buf.WriteByte(c)
		case c == ';':
			stmtNo++
			flush()
		default:
			buf.WriteByte(c)
		}
	}
	if inLiteral {
		return nil, fmt.Errorf("statement %d: %w", stmtNo+1, errUnterminatedLiteral)
	}
	flush()
	return stmts, nil
}
