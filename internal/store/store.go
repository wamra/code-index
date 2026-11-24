// Package store owns the SQLite index: connection setup, schema and the
// sqlite-vec table.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	_ "github.com/asg017/sqlite-vec-go-bindings/ncruces"
	"github.com/ncruces/go-sqlite3"
	_ "github.com/ncruces/go-sqlite3/driver"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
)

// SchemaVersion is meta.schema_version.
const SchemaVersion = 1

// The sqlite-vec wasm build uses atomics, which wazero gates behind the
// threads feature flag. Must be set before the first connection is opened.
func init() { sqlite3.RuntimeConfig = baseConfig() }

func baseConfig() wazero.RuntimeConfig {
	return wazero.NewRuntimeConfig().
		WithCoreFeatures(api.CoreFeaturesV2 | experimental.CoreFeaturesThreads)
}

var cacheOnce sync.Once

// setupCompilationCache points the wazero runtime at an on-disk compilation
// cache in the user cache dir (<cache>/code-index/wazero), never inside a repo,
// so the sqlite wasm is compiled once, not on every process start. Failures
// are non-fatal.
func setupCompilationCache() {
	cacheOnce.Do(func() {
		base, err := os.UserCacheDir()
		if err != nil {
			return
		}
		dir := filepath.Join(base, "code-index", "wazero")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "code-index: compilation cache disabled: %v\n", err)
			return
		}
		cache, err := wazero.NewCompilationCacheWithDir(dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "code-index: compilation cache disabled: %v\n", err)
			return
		}
		sqlite3.RuntimeConfig = baseConfig().WithCompilationCache(cache)
	})
}

//go:embed schema.sql
var schemaSQL string

// Store wraps the index database handle.
type Store struct {
	db   *sql.DB
	path string
}

// JournalMode is the journal mode every writable connection asks for: WAL on
// every platform.
//
// Windows used a rollback journal while the pinned SQLite driver had three
// WAL defects there. go.mod now replaces the driver with a fork
// of the same version that fixes them (github.com/wamra/go-sqlite3, branch
// wamra/v0.21.3-windows-wal).
var JournalMode = journalMode(runtime.GOOS)

func journalMode(goos string) string {
	return "WAL"
}

// dsn builds the "file:" URI for path (percent-encoded, no "//" authority).
func dsn(path string, q url.Values) string {
	return "file:" + (&url.URL{Path: path}).EscapedPath() + "?" + q.Encode()
}

// Open opens (creating if needed) the index at path and applies the schema.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	setupCompilationCache()
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(30000)")
	q.Add("_pragma", "journal_mode("+JournalMode+")")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "foreign_keys(1)")
	db, err := sql.Open("sqlite3", dsn(path, q))
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if _, err := db.Exec(`INSERT OR IGNORE INTO meta(key, value) VALUES ('schema_version', ?)`, strconv.Itoa(SchemaVersion)); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, path: path}, nil
}

// migrate brings a database created by an older release up to the current
// schema. schema.sql only creates missing tables, so a column added to an
// existing table is added here, when PRAGMA table_info does not list it.
func migrate(db *sql.DB) error {
	return addColumn(db, "edges", "reason", "TEXT")
}

// addColumn runs ALTER TABLE ... ADD COLUMN unless the table already has the column.
func addColumn(db *sql.DB, table, column, typ string) error {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		if name == column {
			found = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil || found {
		return err
	}
	return alterAddColumn(db, table, column, typ)
}

// alterAddColumn runs the ALTER TABLE. "duplicate column name" is success: another
// process opening the same old database added the column between our check
// and our ALTER.
func alterAddColumn(db *sql.DB, table, column, typ string) error {
	_, err := db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + typ)
	if err != nil && strings.Contains(err.Error(), "duplicate column name") {
		return nil
	}
	return err
}

// Close (WAL mode) folds the WAL into the main file (PRAGMA wal_checkpoint(TRUNCATE)) so
// the database file is complete on its own, then closes the database. A busy
// checkpoint (another process has it open) is not an error.
func (s *Store) Close() error {
	_, _ = s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	return s.db.Close()
}

// Path returns the database file path given to Open.
func (s *Store) Path() string { return s.path }

// DB returns the underlying handle.
func (s *Store) DB() *sql.DB { return s.db }

// Meta returns meta[key] ("" when unset).
func (s *Store) Meta(ctx context.Context, key string) (string, error) {
	var v sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v.String, err
}

// SetMeta sets meta[key].
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}
