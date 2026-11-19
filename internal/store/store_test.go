package store

import (
	"context"
	"encoding/binary"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/wamra/code-index/internal/embedtest"
)

func open(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "sub", "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestOpenCreatesSchema(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	for _, tbl := range []string{"meta", "files", "symbols", "edges", "summaries", "chunks", "chunk_fts", "name_fts"} {
		var n int
		if err := st.DB().QueryRow(`SELECT count(*) FROM sqlite_master WHERE name=?`, tbl).Scan(&n); err != nil || n != 1 {
			t.Errorf("table %s missing (n=%d err=%v)", tbl, n, err)
		}
	}
	if v, _ := st.Meta(ctx, "schema_version"); v != "1" {
		t.Errorf("schema_version = %q", v)
	}
	// reopening is idempotent
	p := st.Path()
	st.Close()
	st2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	st2.Close()
}

func TestVecRoundTripAndMetadataFilter(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	if err := st.EnsureVec(ctx, 64); err != nil {
		t.Fatal(err)
	}
	if d, _ := st.VecDims(ctx); d != 64 {
		t.Fatalf("dims = %d", d)
	}
	words := map[int64]string{1: "alpha widget", 2: "beta gadget", 3: "alpha gizmo"}
	kinds := map[int64]string{1: "function", 2: "typeparameter", 3: "function"}
	for id, w := range words {
		if _, err := st.DB().Exec(`INSERT INTO chunk_vec(rowid, embedding, kind, lang) VALUES (?,?,?,?)`,
			id, vecBytes(embedtest.Vector(w, 64)), MetaText(kinds[id]), "powershell"); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := st.DB().Query(`SELECT rowid FROM chunk_vec WHERE embedding MATCH ? AND k = 2 ORDER BY distance`, vecBytes(embedtest.Vector("alpha widget", 64)))
	if err != nil {
		t.Fatal(err)
	}
	var first int64
	rows.Next()
	rows.Scan(&first)
	rows.Close()
	if first != 1 {
		t.Errorf("nearest = %d, want 1", first)
	}
	// metadata filter, including a kind longer than 12 bytes (MetaText trims it)
	var id int64
	err = st.DB().QueryRow(`SELECT rowid FROM chunk_vec WHERE embedding MATCH ? AND k = 5 AND kind = ?`,
		vecBytes(embedtest.Vector("beta", 64)), MetaText("typeparameter")).Scan(&id)
	if err != nil || id != 2 {
		t.Errorf("filtered rowid = %d err=%v, want 2", id, err)
	}
}

func TestEnsureVecDimsChangeResetsChunks(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	db := st.DB()
	db.Exec(`INSERT INTO files(id, path, lang, sha256) VALUES (1,'a.sh','bash','x')`)
	db.Exec(`INSERT INTO chunks(id, target_kind, target_id, file_id, text, text_sha256, embedded_at, embed_fingerprint) VALUES (1,'file',1,1,'t','h','2026-01-01T00:00:00Z','fp')`)
	if err := st.EnsureVec(ctx, 8); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureVec(ctx, 8); err != nil { // same size: no-op
		t.Fatal(err)
	}
	var pending int
	db.QueryRow(`SELECT count(*) FROM chunks WHERE embedded_at IS NULL`).Scan(&pending)
	if pending != 0 {
		t.Fatalf("same-size EnsureVec must not reset chunks, pending=%d", pending)
	}
	if err := st.EnsureVec(ctx, 12); err != nil {
		t.Fatal(err)
	}
	db.QueryRow(`SELECT count(*) FROM chunks WHERE embedded_at IS NULL AND embed_fingerprint IS NULL`).Scan(&pending)
	if pending != 1 {
		t.Fatalf("changed size must send chunks back to pending, pending=%d", pending)
	}
	if d, _ := st.VecDims(ctx); d != 12 {
		t.Errorf("dims = %d", d)
	}
}

func TestDeleteFileData(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	db := st.DB()
	if err := st.EnsureVec(ctx, 4); err != nil {
		t.Fatal(err)
	}
	db.Exec(`INSERT INTO files(id, path, lang, sha256) VALUES (1,'a.sh','bash','x')`)
	db.Exec(`INSERT INTO symbols(id, file_id, name, qualified_name, kind) VALUES (1,1,'f','f','function')`)
	db.Exec(`INSERT INTO name_fts(rowid, name, qualified_name, path) VALUES (1,'f','f','a.sh')`)
	db.Exec(`INSERT INTO chunks(id, target_kind, target_id, file_id, text, text_sha256) VALUES (1,'symbol',1,1,'function f','h')`)
	db.Exec(`INSERT INTO chunk_fts(rowid, text) VALUES (1,'function f')`)
	db.Exec(`INSERT INTO chunk_vec(rowid, embedding, kind, lang) VALUES (1, ?, 'function','bash')`, vecBytes([]float32{1, 0, 0, 0}))
	tx, _ := db.BeginTx(ctx, nil)
	if err := DeleteFileData(ctx, tx, 1, true); err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	for _, q := range []string{`symbols`, `chunks`, `name_fts`, `chunk_fts`, `chunk_vec`} {
		var n int
		db.QueryRow(`SELECT count(*) FROM ` + q).Scan(&n)
		if n != 0 {
			t.Errorf("%s still has %d rows", q, n)
		}
	}
	var n int
	db.QueryRow(`SELECT count(*) FROM files`).Scan(&n)
	if n != 1 {
		t.Errorf("files row must stay, got %d", n)
	}
}

func vecBytes(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(f))
	}
	return b
}

func TestJournalModePerOS(t *testing.T) {
	if journalMode("windows") != "DELETE" || journalMode("linux") != "WAL" || journalMode("darwin") != "WAL" {
		t.Error("rollback journal on Windows only")
	}
}

func TestDSNKeepsPathIntact(t *testing.T) {
	q := url.Values{}
	q.Set("mode", "rw")
	for path, want := range map[string]string{
		`/tmp/x/code.sqlite`:     `file:/tmp/x/code.sqlite?mode=rw`,
		`C:\Users\x\code.sqlite`: `file:C:%5CUsers%5Cx%5Ccode.sqlite?mode=rw`,
		`\\srv\share\a.sqlite`:   `file:%5C%5Csrv%5Cshare%5Ca.sqlite?mode=rw`,
		`/tmp/50% #1?/a.sqlite`:  `file:/tmp/50%25%20%231%3F/a.sqlite?mode=rw`,
	} {
		if got := dsn(path, q); got != want {
			t.Errorf("dsn(%q) = %q, want %q", path, got, want)
		}
	}
}

// '#' starts a fragment and '%' an escape in a file: URI; neither may change
// which file is opened.
func TestOpenPathWithURICharacters(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "50%41 #1")
	path := filepath.Join(dir, "code.sqlite")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st.SetMeta(context.Background(), "k", "v")
	st.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database not at the requested path: %v", err)
	}
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if v, _ := st.Meta(context.Background(), "k"); v != "v" {
		t.Errorf("meta k = %q after reopen", v)
	}
}
