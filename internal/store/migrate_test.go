package store

import (
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

// A database created before edges.reason existed (schema-v1.sql is the schema
// of that release) gets the column on Open, keeps its rows, and a second Open
// is a no-op.
func TestOpenAddsEdgesReasonToOldDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.sqlite")
	old, err := os.ReadFile("testdata/schema-v1.sql")
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", dsn(path, url.Values{}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(old)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO files(id, path, lang, sha256) VALUES (1, 'a.sh', 'bash', 'x');
		INSERT INTO edges(kind, file_id, line, col, source, confidence) VALUES ('calls', 1, 0, 0, 'lsp', 'exact')`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('edges') WHERE name='reason'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the old schema must not have reason (n=%d err=%v)", n, err)
	}
	db.Close()

	for i := 0; i < 2; i++ {
		st, err := Open(path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		var reason sql.NullString
		var conf string
		if err := st.DB().QueryRow(`SELECT confidence, reason FROM edges`).Scan(&conf, &reason); err != nil || conf != "exact" || reason.Valid != (i > 0) {
			t.Fatalf("open %d: row = %q %v err=%v", i, conf, reason, err)
		}
		if _, err := st.DB().Exec(`UPDATE edges SET reason='direct'`); err != nil {
			t.Fatal(err)
		}
		st.Close()
	}
}

// Two processes opening an old database at once can both see the column
// missing; the loser's ALTER fails with "duplicate column name", which means
// the column exists and is not an error.
func TestAlterAddColumnToleratesDuplicateColumn(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "x.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// edges.reason exists after Open: this ALTER is what the losing process runs
	if err := alterAddColumn(st.DB(), "edges", "reason", "TEXT"); err != nil {
		t.Fatalf("duplicate column must be treated as success: %v", err)
	}
	// other ALTER failures are still errors
	if err := alterAddColumn(st.DB(), "no_such_table", "c", "TEXT"); err == nil {
		t.Fatal("an ALTER on a missing table must still fail")
	}
	if err := addColumn(st.DB(), "edges", "reason", "TEXT"); err != nil {
		t.Fatalf("addColumn on an existing column: %v", err)
	}
}
