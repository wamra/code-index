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
	"strconv"
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
// cache (<db dir>/wazero-cache) so the sqlite wasm is compiled once, not on
// every process start. Failures are non-fatal.
func setupCompilationCache(dbPath string) {
	cacheOnce.Do(func() {
		dir := filepath.Join(filepath.Dir(dbPath), "wazero-cache")
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

// dsn builds the "file:" URI for path (percent-encoded, no "//" authority).
func dsn(path string, q url.Values) string {
	return "file:" + (&url.URL{Path: path}).EscapedPath() + "?" + q.Encode()
}

// Open opens (creating if needed) the index at path and applies the schema.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	setupCompilationCache(path)
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(30000)")
	q.Add("_pragma", "journal_mode(WAL)")
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
	if _, err := db.Exec(`INSERT OR IGNORE INTO meta(key, value) VALUES ('schema_version', ?)`, strconv.Itoa(SchemaVersion)); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, path: path}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

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
