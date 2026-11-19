package embed

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/wamra/code-index/internal/filelock"
	"github.com/wamra/code-index/internal/provider"
	"github.com/wamra/code-index/internal/store"
)

const (
	maxBatch   = 128
	failedMark = "failed"
)

// maxBatchBytes caps the text bytes per batch.
var maxBatchBytes = 1 << 20

type pending struct {
	id         int64
	text       string
	kind, lang string
}

// Pending counts chunks awaiting an embedding.
func Pending(ctx context.Context, st *store.Store) (int, error) {
	var n int
	err := st.DB().QueryRowContext(ctx, `SELECT count(*) FROM chunks WHERE embedded_at IS NULL`).Scan(&n)
	return n, err
}

// ErrDailyCap is returned by Drain when embed.max_items_per_day is reached;
// the remaining chunks stay pending.
type ErrDailyCap struct{ Used, Cap int }

func (e *ErrDailyCap) Error() string {
	return fmt.Sprintf("daily cap reached: %d of %d chunks embedded today (UTC); remaining chunks stay pending", e.Used, e.Cap)
}

// Opts tunes Drain.
type Opts struct {
	Batch     int
	MaxPerDay int // 0 = unlimited
	Now       func() time.Time
}

// Drain embeds all pending chunks and returns how many were embedded.
//
// chunk_vec is created at the embedder's vector size. A different size, or a
// different embedder fingerprint than the one recorded in meta, sends every
// chunk back to pending first, so vectors of two models are never mixed.
func Drain(ctx context.Context, st *store.Store, c provider.Embedder, o Opts) (int, error) {
	now := o.Now
	if now == nil {
		now = time.Now
	}
	batch := o.Batch
	if batch <= 0 || batch > maxBatch {
		batch = maxBatch
	}
	l, err := filelock.Try(filepath.Join(filepath.Dir(st.Path()), "embed.lock"))
	if errors.Is(err, filelock.ErrLocked) {
		return 0, ErrLocked
	}
	if err != nil {
		return 0, err
	}
	defer l.Release()

	if c.Dims() == 0 { // unknown size: detect it with one probe request
		if _, err := c.Embed(ctx, []string{"dimension probe"}, ""); err != nil {
			return 0, err
		}
		if c.Dims() == 0 {
			return 0, fmt.Errorf("%w: could not detect the embedding size", provider.ErrBroken)
		}
	}
	if err := st.EnsureVec(ctx, c.Dims()); err != nil {
		return 0, err
	}
	stored, err := st.Meta(ctx, "embed_fingerprint")
	if err != nil {
		return 0, err
	}
	if stored != c.Fingerprint() {
		if stored != "" {
			if err := st.ResetVectors(ctx); err != nil {
				return 0, err
			}
		}
		if err := st.SetMeta(ctx, "embed_fingerprint", c.Fingerprint()); err != nil {
			return 0, err
		}
	}

	total := 0
	for ctx.Err() == nil {
		limit := batch
		if o.MaxPerDay > 0 {
			used, err := store.UsageToday(ctx, st.DB(), store.UsageEmbed, now())
			if err != nil {
				return total, err
			}
			if used >= o.MaxPerDay {
				if n, err := Pending(ctx, st); err != nil {
					return total, err
				} else if n > 0 {
					return total, &ErrDailyCap{Used: used, Cap: o.MaxPerDay}
				}
				break
			}
			limit = min(limit, o.MaxPerDay-used)
		}
		rows, err := loadPending(ctx, st.DB(), limit)
		if err != nil {
			return total, err
		}
		if len(rows) == 0 {
			break
		}
		// counted before the request so a failed batch still counts
		if err := store.AddUsage(context.WithoutCancel(ctx), st.DB(), store.UsageEmbed, len(rows), now()); err != nil {
			return total, err
		}
		good, vecs, bad, err := embedBisect(ctx, c, rows)
		if err != nil {
			return total, err
		}
		if err := commitBatch(ctx, st.DB(), c.Fingerprint(), good, vecs, bad); err != nil {
			return total, err
		}
		total += len(good)
	}
	return total, ctx.Err()
}

func loadPending(ctx context.Context, db *sql.DB, limit int) ([]pending, error) {
	rs, err := db.QueryContext(ctx, `SELECT c.id, c.text, COALESCE(s.kind, c.target_kind), f.lang
FROM chunks c JOIN files f ON f.id = c.file_id
LEFT JOIN symbols s ON c.target_kind = 'symbol' AND s.id = c.target_id
WHERE c.embedded_at IS NULL ORDER BY c.id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []pending
	size := 0
	for rs.Next() {
		var p pending
		if err := rs.Scan(&p.id, &p.text, &p.kind, &p.lang); err != nil {
			return nil, err
		}
		p.text = provider.CapRunes(p.text, provider.MaxChunk)
		if len(out) > 0 && size+len(p.text) > maxBatchBytes {
			break
		}
		size += len(p.text)
		out = append(out, p)
	}
	return out, rs.Err()
}

func embedBisect(ctx context.Context, c provider.Embedder, rows []pending) (good []pending, vecs [][]float32, bad []pending, err error) {
	good, vecs, bad, err = bisect(ctx, c, rows)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(good) == 0 && len(bad) > 1 {
		return nil, nil, nil, fmt.Errorf("%w: every row in a batch of %d failed", provider.ErrBroken, len(rows))
	}
	return good, vecs, bad, nil
}

func bisect(ctx context.Context, c provider.Embedder, rows []pending) (good []pending, vecs [][]float32, bad []pending, err error) {
	texts := make([]string, len(rows))
	for i, r := range rows {
		texts[i] = r.text
	}
	v, err := c.Embed(ctx, texts, provider.DocPrefixOf(c))
	if err == nil {
		for _, x := range v {
			if len(x) != c.Dims() {
				return nil, nil, nil, fmt.Errorf("%w: embedding dimension %d, expected %d", provider.ErrBroken, len(x), c.Dims())
			}
		}
		return rows, v, nil, nil
	}
	if provider.IsSystemic(err) {
		return nil, nil, nil, err
	}
	if ctx.Err() != nil {
		return nil, nil, nil, ctx.Err()
	}
	if len(rows) == 1 {
		return nil, nil, rows, nil
	}
	mid := len(rows) / 2
	g1, v1, b1, err := bisect(ctx, c, rows[:mid])
	if err != nil {
		return nil, nil, nil, err
	}
	g2, v2, b2, err := bisect(ctx, c, rows[mid:])
	if err != nil {
		return nil, nil, nil, err
	}
	return append(append([]pending{}, g1...), g2...), append(append([][]float32{}, v1...), v2...), append(append([]pending{}, b1...), b2...), nil
}

func commitBatch(ctx context.Context, db *sql.DB, fp string, good []pending, vecs [][]float32, bad []pending) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339)
	for i, p := range good {
		if _, err := tx.ExecContext(ctx, `DELETE FROM chunk_vec WHERE rowid=?`, p.id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO chunk_vec(rowid, embedding, kind, lang) VALUES (?, ?, ?, ?)`,
			p.id, VecBytes(vecs[i]), store.MetaText(p.kind), store.MetaText(p.lang)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE chunks SET embedded_at=?, embed_fingerprint=? WHERE id=?`, now, fp, p.id); err != nil {
			return err
		}
	}
	for _, p := range bad {
		if _, err := tx.ExecContext(ctx, `UPDATE chunks SET embedded_at=? WHERE id=?`, failedMark, p.id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
