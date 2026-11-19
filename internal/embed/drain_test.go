package embed_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/wamra/code-index/internal/embed"
	"github.com/wamra/code-index/internal/embedtest"
	"github.com/wamra/code-index/internal/filelock"
	"github.com/wamra/code-index/internal/provider"
	"github.com/wamra/code-index/internal/store"
)

func newStore(t *testing.T, n int) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	db := st.DB()
	db.Exec(`INSERT INTO files(id, path, lang, sha256) VALUES (1,'a.sh','bash','x')`)
	for i := 1; i <= n; i++ {
		db.Exec(`INSERT INTO symbols(id, file_id, name, qualified_name, kind) VALUES (?,1,?,?,'function')`, i, fmt.Sprint("f", i), fmt.Sprint("f", i))
		db.Exec(`INSERT INTO chunks(id, target_kind, target_id, file_id, text, text_sha256) VALUES (?, 'symbol', ?, 1, ?, 'h')`, i, i, fmt.Sprintf("function f%d widget%d", i, i))
	}
	return st
}

func embedder(t *testing.T, url, model string, dims int) provider.Embedder {
	t.Helper()
	e, err := provider.NewEmbedder(provider.EmbedConfig{
		Provider: provider.Ollama, BaseURL: url, Model: model, Dims: dims,
		DocPrefix: provider.Str(""), QueryPrefix: provider.Str(""),
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func count(t *testing.T, st *store.Store, q string) int {
	var n int
	if err := st.DB().QueryRow(q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDrainEmbedsPendingChunksOnce(t *testing.T) {
	srv := embedtest.New(32, "m")
	defer srv.Close()
	st := newStore(t, 5)
	e := embedder(t, srv.URL, "m", 32)
	n, err := embed.Drain(context.Background(), st, e, embed.Opts{})
	if err != nil || n != 5 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if got := count(t, st, `SELECT count(*) FROM chunk_vec`); got != 5 {
		t.Errorf("chunk_vec rows = %d", got)
	}
	if got := count(t, st, `SELECT count(*) FROM chunks WHERE embedded_at IS NOT NULL AND embed_fingerprint='`+e.Fingerprint()+`'`); got != 5 {
		t.Errorf("embedded chunks with fingerprint = %d", got)
	}
	if fp, _ := st.Meta(context.Background(), "embed_fingerprint"); fp != e.Fingerprint() {
		t.Errorf("meta fingerprint = %q", fp)
	}
	before := srv.Texts.Load()
	if n, err := embed.Drain(context.Background(), st, e, embed.Opts{}); err != nil || n != 0 || srv.Texts.Load() != before {
		t.Errorf("second drain: n=%d err=%v extra texts=%d", n, err, srv.Texts.Load()-before)
	}
}

func TestChangedFingerprintReembedsEverything(t *testing.T) {
	srv := embedtest.New(32, "m1", "m2")
	defer srv.Close()
	st := newStore(t, 3)
	ctx := context.Background()
	if _, err := embed.Drain(ctx, st, embedder(t, srv.URL, "m1", 32), embed.Opts{}); err != nil {
		t.Fatal(err)
	}
	e2 := embedder(t, srv.URL, "m2", 32) // same size, other model
	n, err := embed.Drain(ctx, st, e2, embed.Opts{})
	if err != nil || n != 3 {
		t.Fatalf("n=%d err=%v, want all 3 re-embedded", n, err)
	}
	if got := count(t, st, `SELECT count(*) FROM chunks WHERE embed_fingerprint <> '`+e2.Fingerprint()+`'`); got != 0 {
		t.Errorf("%d chunks still carry the old fingerprint", got)
	}
	// a different vector size recreates chunk_vec
	srv48 := embedtest.New(48, "m2")
	defer srv48.Close()
	n, err = embed.Drain(ctx, st, embedder(t, srv48.URL, "m2", 48), embed.Opts{})
	if err != nil || n != 3 {
		t.Fatalf("dims change: n=%d err=%v", n, err)
	}
	if d, _ := st.VecDims(ctx); d != 48 {
		t.Errorf("vec dims = %d", d)
	}
}

func TestDrainProviderDownLeavesChunksPending(t *testing.T) {
	srv := embedtest.New(16, "m")
	defer srv.Close()
	srv.Down.Store(true)
	st := newStore(t, 2)
	_, err := embed.Drain(context.Background(), st, embedder(t, srv.URL, "m", 16), embed.Opts{})
	if !errors.Is(err, provider.ErrDown) {
		t.Fatalf("err = %v, want ErrDown", err)
	}
	if got := count(t, st, `SELECT count(*) FROM chunks WHERE embedded_at IS NULL`); got != 2 {
		t.Errorf("pending = %d, want 2 (nothing parked)", got)
	}
}

func TestDailyCap(t *testing.T) {
	srv := embedtest.New(16, "m")
	defer srv.Close()
	st := newStore(t, 5)
	now := func() time.Time { return time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC) }
	e := embedder(t, srv.URL, "m", 16)
	n, err := embed.Drain(context.Background(), st, e, embed.Opts{MaxPerDay: 3, Now: now})
	var capErr *embed.ErrDailyCap
	if !errors.As(err, &capErr) || n != 3 {
		t.Fatalf("n=%d err=%v, want 3 embedded and ErrDailyCap", n, err)
	}
	if pending, _ := embed.Pending(context.Background(), st); pending != 2 {
		t.Errorf("pending = %d", pending)
	}
	// next UTC day: the rest goes through
	next := func() time.Time { return now().Add(24 * time.Hour) }
	if n, err := embed.Drain(context.Background(), st, e, embed.Opts{MaxPerDay: 3, Now: next}); err != nil || n != 2 {
		t.Errorf("next day: n=%d err=%v", n, err)
	}
}

func TestDrainLocked(t *testing.T) {
	srv := embedtest.New(16, "m")
	defer srv.Close()
	st := newStore(t, 1)
	l, err := filelock.Try(filepath.Join(filepath.Dir(st.Path()), "embed.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	if _, err := embed.Drain(context.Background(), st, embedder(t, srv.URL, "m", 16), embed.Opts{}); !errors.Is(err, embed.ErrLocked) {
		t.Errorf("err = %v, want ErrLocked", err)
	}
}

func TestUnknownDimsAreDetected(t *testing.T) {
	srv := embedtest.New(24, "m")
	defer srv.Close()
	st := newStore(t, 2)
	e := embedder(t, srv.URL, "m", 0)
	if e.Dims() != 0 {
		t.Fatalf("dims = %d, want 0 before the probe", e.Dims())
	}
	if n, err := embed.Drain(context.Background(), st, e, embed.Opts{}); err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if d, _ := st.VecDims(context.Background()); d != 24 {
		t.Errorf("vec dims = %d", d)
	}
}
