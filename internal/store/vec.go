package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
)

// CreateChunkVecSQL returns the statement that creates chunk_vec with dims.
// kind and lang are metadata columns so filters run inside the KNN.
func CreateChunkVecSQL(dims int) string {
	return fmt.Sprintf(`CREATE VIRTUAL TABLE IF NOT EXISTS chunk_vec USING vec0(
  embedding float[%d],
  kind text,
  lang text
)`, dims)
}

// VecDims returns meta.vec_dims (0 when chunk_vec was never created).
func (s *Store) VecDims(ctx context.Context) (int, error) {
	v, err := s.Meta(ctx, "vec_dims")
	if err != nil || v == "" {
		return 0, err
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("meta.vec_dims %q: %w", v, err)
	}
	return n, nil
}

// EnsureVec makes chunk_vec exist with dims. When it exists with another
// size it is dropped and recreated empty, and every chunk goes back to
// pending (a changed embedder never leaves a mix of models).
func (s *Store) EnsureVec(ctx context.Context, dims int) error {
	stored, err := s.VecDims(ctx)
	if err != nil {
		return err
	}
	if stored == dims {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if stored != 0 {
		if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS chunk_vec`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE chunks SET embedded_at=NULL, embed_fingerprint=NULL`); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, CreateChunkVecSQL(dims)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES('vec_dims', ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, strconv.Itoa(dims)); err != nil {
		return err
	}
	return tx.Commit()
}

// ResetVectors empties chunk_vec and marks every chunk pending.
func (s *Store) ResetVectors(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if d, _ := s.VecDims(ctx); d > 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM chunk_vec`); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE chunks SET embedded_at=NULL, embed_fingerprint=NULL`); err != nil {
		return err
	}
	return tx.Commit()
}

// FTSCommand runs an FTS5 special command ('rebuild', 'integrity-check', ...).
func FTSCommand(ctx context.Context, db *sql.DB, table, cmd string) error {
	if _, err := db.ExecContext(ctx, `INSERT INTO "`+table+`"("`+table+`") VALUES('`+cmd+`')`); err != nil {
		return fmt.Errorf("%s %s: %w", table, cmd, err)
	}
	return nil
}

// MetaText fits s into a vec0 text metadata column (at most 12 bytes), so
// inserts and filters agree.
func MetaText(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// DeleteFileData removes a file's symbols, edges, chunks and their FTS and
// vector rows inside tx (the files row itself stays). hasVec says whether
// chunk_vec exists.
func DeleteFileData(ctx context.Context, tx *sql.Tx, fileID int64, hasVec bool) error {
	stmts := []string{
		`DELETE FROM name_fts WHERE rowid IN (SELECT id FROM symbols WHERE file_id=?1)`,
		`DELETE FROM chunk_fts WHERE rowid IN (SELECT id FROM chunks WHERE file_id=?1)`,
	}
	if hasVec {
		stmts = append(stmts, `DELETE FROM chunk_vec WHERE rowid IN (SELECT id FROM chunks WHERE file_id=?1)`)
	}
	stmts = append(stmts,
		`DELETE FROM summaries WHERE target_kind='symbol' AND target_id IN (SELECT id FROM symbols WHERE file_id=?1)`,
		`DELETE FROM summaries WHERE target_kind='file' AND target_id=?1`,
		`DELETE FROM edges WHERE file_id=?1`,
		`DELETE FROM chunks WHERE file_id=?1`,
		`DELETE FROM symbols WHERE file_id=?1`,
	)
	for _, q := range stmts {
		if _, err := tx.ExecContext(ctx, q, fileID); err != nil {
			return err
		}
	}
	return nil
}
