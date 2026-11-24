// Package indexer walks a workspace, asks language servers for symbols and
// writes files, symbols and chunks to the store.
package indexer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/wamra/code-index/internal/config"
	"github.com/wamra/code-index/internal/filelock"
	"github.com/wamra/code-index/internal/lsp"
	"github.com/wamra/code-index/internal/store"
)

// ErrLocked means another code-index run holds the database lock.
var ErrLocked = errors.New("another code-index run holds the lock for this database")

// Options tunes Run.
type Options struct {
	Root  string // absolute workspace root
	Force bool   // re-index files even when unchanged
	Log   io.Writer
	// Open starts a language server; nil means lsp.Start.
	Open func(lsp.Spec) (*lsp.Client, error)
	// EmptyRetryDelays overrides the waits before an lsp extractor re-asks for
	// symbols after an empty answer (nil = extract.EmptyRetryDelays).
	EmptyRetryDelays []time.Duration
	// NoEdges skips call-graph edges (LSP references and AST extractors).
	NoEdges bool
	// Extract runs an AST extractor; nil means the embedded script run with pwsh.
	Extract ExtractFunc
}

// Result summarizes a run.
type Result struct {
	Scanned   int // files of configured languages found
	Indexed   int
	Unchanged int
	Failed    int
	Deleted   int
	TooLarge  int
	Binary    int
	Symbols   int
	Chunks    int
	// LangErrors holds a message per language whose server could not be used.
	LangErrors map[string]string
	// FileErrors lists "path: reason" for files that failed.
	FileErrors []string
	// Edges counts the edges written, Aliases the alias symbols the AST
	// extractor created, in the languages whose edges were recomputed.
	Edges, Aliases, EdgeLangs int
	// Notes are one-line remarks about skipped optional work (no pwsh, ...).
	Notes   []string
	Elapsed time.Duration
}

type fileRow struct {
	id     int64
	sha    string
	mtime  int64
	size   int64
	status string
	lang   string
}

func (o Options) logf(format string, a ...any) {
	if o.Log != nil {
		fmt.Fprintf(o.Log, format+"\n", a...)
	}
}

// Run indexes the workspace: new and changed files (by sha256) are sent to
// their language server, deleted files and files that became ignored are
// dropped. The index is not touched for languages whose server cannot start.
func Run(ctx context.Context, st *store.Store, cfg config.Config, o Options) (Result, error) {
	start := time.Now()
	res := Result{LangErrors: map[string]string{}}
	root := o.Root

	lock, err := filelock.Try(filepath.Join(filepath.Dir(st.Path()), "index.lock"))
	if errors.Is(err, filelock.ErrLocked) {
		return res, ErrLocked
	}
	if err != nil {
		return res, err
	}
	defer lock.Release()

	if prev, err := st.Meta(ctx, "root"); err != nil {
		return res, err
	} else if prev != "" && prev != root {
		return res, fmt.Errorf("this database indexes %s, not %s", prev, root)
	}
	if err := st.SetMeta(ctx, "root", root); err != nil {
		return res, err
	}

	cands, ws, err := walk(root, cfg)
	if err != nil {
		return res, err
	}
	res.TooLarge = ws.TooLarge
	res.Scanned = len(cands)

	existing, err := loadFiles(ctx, st.DB())
	if err != nil {
		return res, err
	}
	hasVec := false
	if d, err := st.VecDims(ctx); err == nil && d > 0 {
		hasVec = true
	}

	// Cache key of each language's edges (docs/extractors.md section 5). The
	// stored value is "" (edges only), the current fingerprint (up to date) or
	// anything else (an older fingerprint or the legacy "1"): the language is
	// fully stale and all its files are re-indexed.
	fpMemo := map[string]string{}
	fingerprint := func(lang string) string {
		if _, ok := fpMemo[lang]; !ok {
			fpMemo[lang] = langFingerprint(ctx, cfg, lang, root)
		}
		return fpMemo[lang]
	}
	candLangs := map[string]bool{}
	for _, c := range cands {
		candLangs[c.lang] = true
	}
	storedEdges := map[string]string{}
	fullStale := map[string]bool{}
	for l := range candLangs {
		stored, err := st.Meta(ctx, edgesMetaKey(l))
		if err != nil {
			return res, err
		}
		storedEdges[l] = stored
		fullStale[l] = stored != "" && stored != fingerprint(l)
	}

	// Decide per file: unchanged, or needs indexing.
	byLang := map[string][]workItem{}
	seen := make(map[string]bool, len(cands))
	for _, c := range cands {
		seen[c.rel] = true
		row, known := existing[c.rel]
		reindex := o.Force || fullStale[c.lang]
		if known && !reindex && row.status == "ok" && row.lang == c.lang && row.size == c.size && row.mtime == c.mtime {
			res.Unchanged++
			continue
		}
		data, err := os.ReadFile(c.abs)
		if err != nil {
			res.Failed++
			res.FileErrors = append(res.FileErrors, c.rel+": "+err.Error())
			continue
		}
		if looksBinary(data) {
			res.Binary++
			continue
		}
		sha := sha256Hex(data)
		if known && !reindex && row.status == "ok" && row.lang == c.lang && row.sha == sha {
			if _, err := st.DB().ExecContext(ctx, `UPDATE files SET mtime=?, size=? WHERE id=?`, c.mtime, c.size, row.id); err != nil {
				return res, err
			}
			res.Unchanged++
			continue
		}
		byLang[c.lang] = append(byLang[c.lang], workItem{c, data, sha})
	}

	// Drop files that vanished, became ignored, or lost their language.
	deletedLangs := map[string]bool{}
	for rel, row := range existing {
		if seen[rel] {
			continue
		}
		if err := dropFile(ctx, st.DB(), row.id, hasVec); err != nil {
			return res, err
		}
		res.Deleted++
		deletedLangs[row.lang] = true
	}

	// Languages whose edges need recomputing: any language with changed or
	// deleted files, every language on --force, and a language whose edges
	// were never computed (or were left stale by a --no-edges run).
	edgeLangs := map[string]bool{}
	for l := range byLang {
		edgeLangs[l] = true
	}
	for l := range deletedLangs {
		edgeLangs[l] = true
	}
	for l := range candLangs {
		if o.Force || storedEdges[l] != fingerprint(l) {
			edgeLangs[l] = true
		}
	}
	for l := range edgeLangs {
		if _, ok := cfg.Languages[l]; !ok || o.NoEdges {
			// A fully stale language is marked after its files are re-indexed (below).
			if o.NoEdges && !fullStale[l] && (len(byLang[l]) > 0 || deletedLangs[l] || o.Force) {
				_ = st.SetMeta(ctx, edgesMetaKey(l), "") // stale now: recompute on the next run
			}
			delete(edgeLangs, l)
		}
	}

	langSet := map[string]bool{}
	for l := range byLang {
		langSet[l] = true
	}
	for l := range edgeLangs {
		langSet[l] = true
	}
	langs := make([]string, 0, len(langSet))
	for l := range langSet {
		langs = append(langs, l)
	}
	sort.Strings(langs)
	// edgesNotDone marks a language whose edges were not computed. A fully stale
	// language keeps its old value, so the next run still finds it stale and
	// re-indexes the files this run did not reach.
	edgesNotDone := func(lang string) {
		if !fullStale[lang] {
			_ = st.SetMeta(ctx, edgesMetaKey(lang), "")
		}
	}
	edgesRan := false
	for _, lang := range langs {
		items := byLang[lang]
		lc := cfg.Languages[lang]
		live, notes, serr := startSessions(ctx, o, root, lang, cfg.ExtractorsFor(lang), len(items) > 0, edgeLangs[lang])
		res.Notes = append(res.Notes, notes...)
		if serr != nil {
			closeSessions(live)
			if ctx.Err() != nil {
				break
			}
			if len(items) > 0 {
				res.LangErrors[lang] = serr.Error()
				res.Failed += len(items)
				o.logf("%s: %d file(s) not indexed: %v", lang, len(items), serr)
			} else {
				res.Notes = append(res.Notes, fmt.Sprintf("%s: %v; edges not computed", lang, serr))
			}
			edgesNotDone(lang)
			continue
		}
		if len(items) > 0 {
			sym := symbolSession(live)
			o.logf("%s: %s ready, indexing %d file(s)", lang, sym.ne.Name, len(items))
			indexItems(ctx, st, o, lang, sym, items, existing, hasVec, &res)
		}
		complete := ctx.Err() == nil && res.LangErrors[lang] == ""
		if edgeLangs[lang] && complete {
			es, err := computeEdges(ctx, st, o, root, lang, live, lc.Extensions)
			res.Notes = append(res.Notes, es.Notes...)
			if err != nil {
				res.Notes = append(res.Notes, fmt.Sprintf("%s: edges not computed: %v", lang, err))
				_ = st.SetMeta(ctx, edgesMetaKey(lang), "") // the files of this run are current: edges only next time
			} else {
				res.Edges += es.Edges
				res.Aliases += es.Aliases
				res.EdgeLangs++
				edgesRan = true
				o.logf("  %s: %d edge(s), %d alias(es)", lang, es.Edges, es.Aliases)
				_ = st.SetMeta(ctx, edgesMetaKey(lang), fingerprint(lang))
			}
		} else if edgeLangs[lang] {
			edgesNotDone(lang)
		} else if o.NoEdges && fullStale[lang] && complete {
			// --no-edges re-indexed a stale language: the next run rebuilds its edges only.
			_ = st.SetMeta(ctx, edgesMetaKey(lang), "")
		}
		closeSessions(live)
	}
	if err := ctx.Err(); err != nil {
		res.Elapsed = time.Since(start)
		return res, err
	}
	if edgesRan {
		n, err := refreshLinks(ctx, st)
		if err != nil {
			return res, err
		}
		o.logf("calls / called by lines changed in %d chunk(s)", n)
	}
	if err := st.SetMeta(ctx, "last_index_at", time.Now().UTC().Format(time.RFC3339)); err != nil {
		return res, err
	}
	res.Elapsed = time.Since(start)
	return res, nil
}

func edgesMetaKey(lang string) string { return "edges_lang:" + lang }

func loadFiles(ctx context.Context, db *sql.DB) (map[string]fileRow, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, path, sha256, COALESCE(mtime,0), COALESCE(size,0), status, lang FROM files`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]fileRow{}
	for rows.Next() {
		var r fileRow
		var p string
		if err := rows.Scan(&r.id, &p, &r.sha, &r.mtime, &r.size, &r.status, &r.lang); err != nil {
			return nil, err
		}
		out[p] = r
	}
	return out, rows.Err()
}

func dropFile(ctx context.Context, db *sql.DB, id int64, hasVec bool) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := store.DeleteFileData(ctx, tx, id, hasVec); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM files WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

type fileStats struct{ symbols, chunks int }

func recordFailure(ctx context.Context, st *store.Store, c candidate, sha string, oldID int64, hasVec bool, cause error) error {
	tx, err := st.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := upsertFile(ctx, tx, c, sha, oldID, hasVec, "failed", cause.Error()); err != nil {
		return err
	}
	return tx.Commit()
}

func upsertFile(ctx context.Context, tx *sql.Tx, c candidate, sha string, oldID int64, hasVec bool, status, reason string) (int64, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	if oldID != 0 {
		if err := store.DeleteFileData(ctx, tx, oldID, hasVec); err != nil {
			return 0, err
		}
		_, err := tx.ExecContext(ctx, `UPDATE files SET lang=?, sha256=?, mtime=?, size=?, indexed_at=?, status=?, fail_reason=? WHERE id=?`,
			c.lang, sha, c.mtime, c.size, now, status, nullIf(reason), oldID)
		return oldID, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO files(path, lang, sha256, mtime, size, indexed_at, status, fail_reason) VALUES (?,?,?,?,?,?,?,?)`,
		c.rel, c.lang, sha, c.mtime, c.size, now, status, nullIf(reason))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func nullIf(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Values of symbols.source.
const (
	symSourceLSP     = "lsp"     // from a language server's documentSymbol
	symSourceAST     = "ast"     // alias symbols made with the edges; deleteLangEdges removes them on every rebuild
	symSourceExtract = "extract" // from the symbol/func records of an extractor
)

func writeFile(ctx context.Context, st *store.Store, c candidate, sha string, oldID int64, hasVec bool, syms []flatSym) (fileStats, error) {
	return writeFileSource(ctx, st, c, sha, oldID, hasVec, syms, symSourceLSP)
}

// writeFileSource is writeFile with the symbols.source value to store.
func writeFileSource(ctx context.Context, st *store.Store, c candidate, sha string, oldID int64, hasVec bool, syms []flatSym, source string) (fileStats, error) {
	var fs fileStats
	tx, err := st.DB().BeginTx(ctx, nil)
	if err != nil {
		return fs, err
	}
	defer tx.Rollback()
	fileID, err := upsertFile(ctx, tx, c, sha, oldID, hasVec, "ok", "")
	if err != nil {
		return fs, err
	}
	ids := make([]int64, len(syms))
	for i, s := range syms {
		var parent any
		if s.Parent >= 0 {
			parent = ids[s.Parent]
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO symbols(file_id, name, qualified_name, kind, parent_id,
			start_line, start_col, end_line, end_col, sel_line, sel_col, signature, doc, body_sha256, source)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			fileID, s.Name, s.Qualified, s.Kind, parent,
			s.Range.Start.Line, s.Range.Start.Character, s.Range.End.Line, s.Range.End.Character,
			s.SelectionRange.Start.Line, s.SelectionRange.Start.Character,
			nullIf(s.Signature), nullIf(s.Doc), s.BodySHA, source)
		if err != nil {
			return fs, err
		}
		ids[i], _ = res.LastInsertId()
		if _, err := tx.ExecContext(ctx, `INSERT INTO name_fts(rowid, name, qualified_name, path) VALUES (?,?,?,?)`,
			ids[i], s.Name, s.Qualified, c.rel); err != nil {
			return fs, err
		}
		fs.symbols++
		if chunkSkipKinds[s.Kind] {
			continue
		}
		if err := addChunk(ctx, tx, "symbol", ids[i], fileID, SymbolChunk(s.Kind, s.Qualified, c.rel, s.Signature, s.Doc)); err != nil {
			return fs, err
		}
		fs.chunks++
	}
	if err := addChunk(ctx, tx, "file", fileID, fileID, FileChunk(c.rel, c.lang, syms)); err != nil {
		return fs, err
	}
	fs.chunks++
	return fs, tx.Commit()
}

func addChunk(ctx context.Context, tx *sql.Tx, targetKind string, targetID, fileID int64, text string) error {
	res, err := tx.ExecContext(ctx, `INSERT INTO chunks(target_kind, target_id, file_id, text, text_sha256) VALUES (?,?,?,?,?)`,
		targetKind, targetID, fileID, text, sha256Hex([]byte(text)))
	if err != nil {
		return err
	}
	id, _ := res.LastInsertId()
	_, err = tx.ExecContext(ctx, `INSERT INTO chunk_fts(rowid, text) VALUES (?,?)`, id, text)
	return err
}
