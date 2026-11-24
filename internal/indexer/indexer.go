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
	"strings"
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
	// EmptyRetryDelays overrides the waits before re-asking for symbols after
	// an empty answer (nil = DefaultEmptyRetryDelays).
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

	// Decide per file: unchanged, or needs indexing.
	type work struct {
		c    candidate
		data []byte
		sha  string
	}
	byLang := map[string][]work{}
	seen := make(map[string]bool, len(cands))
	for _, c := range cands {
		seen[c.rel] = true
		row, known := existing[c.rel]
		if known && !o.Force && row.status == "ok" && row.lang == c.lang && row.size == c.size && row.mtime == c.mtime {
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
		if known && !o.Force && row.status == "ok" && row.lang == c.lang && row.sha == sha {
			if _, err := st.DB().ExecContext(ctx, `UPDATE files SET mtime=?, size=? WHERE id=?`, c.mtime, c.size, row.id); err != nil {
				return res, err
			}
			res.Unchanged++
			continue
		}
		byLang[c.lang] = append(byLang[c.lang], work{c, data, sha})
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
	candLangs := map[string]bool{}
	for _, c := range cands {
		candLangs[c.lang] = true
	}
	for l := range byLang {
		edgeLangs[l] = true
	}
	for l := range deletedLangs {
		edgeLangs[l] = true
	}
	for l := range candLangs {
		if done, _ := st.Meta(ctx, edgesMetaKey(l)); o.Force || done != "1" {
			edgeLangs[l] = true
		}
	}
	for l := range edgeLangs {
		if _, ok := cfg.Languages[l]; !ok || o.NoEdges {
			if o.NoEdges && (len(byLang[l]) > 0 || deletedLangs[l] || o.Force) {
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
	edgesRan := false
	for _, lang := range langs {
		items := byLang[lang]
		lc := cfg.Languages[lang]
		cl, err := startServer(ctx, o, lc, root)
		if err != nil {
			if len(items) > 0 {
				res.LangErrors[lang] = err.Error()
				res.Failed += len(items)
				o.logf("%s: %d file(s) not indexed: %v", lang, len(items), err)
				_ = st.SetMeta(ctx, edgesMetaKey(lang), "")
				continue
			}
			// nothing to re-index: still build the edges the AST extractor can give
			res.Notes = append(res.Notes, fmt.Sprintf("%s: no language server (%v); LSP edges skipped", lang, err))
			cl = nil
		} else {
			o.logf("%s: %s %s ready, indexing %d file(s)", lang, orDash(cl.ServerName), cl.ServerVersion, len(items))
		}
		rs := &retryState{delays: o.EmptyRetryDelays}
		if rs.delays == nil {
			rs.delays = DefaultEmptyRetryDelays
		}
		for _, w := range items {
			if ctx.Err() != nil {
				break
			}
			fs, err := indexFile(ctx, st, cl, rs, lc, w.c, w.data, w.sha, existing[w.c.rel].id, hasVec)
			if err != nil {
				res.Failed++
				res.FileErrors = append(res.FileErrors, w.c.rel+": "+err.Error())
				o.logf("  failed %s: %v", w.c.rel, err)
				if errors.Is(err, lsp.ErrClosed) { // server died: stop this language
					res.LangErrors[lang] = err.Error()
					break
				}
				continue
			}
			res.Indexed++
			res.Symbols += fs.symbols
			res.Chunks += fs.chunks
			o.logf("  %s: %d symbol(s)", w.c.rel, fs.symbols)
		}
		if edgeLangs[lang] && ctx.Err() == nil && res.LangErrors[lang] == "" {
			es, err := computeEdges(ctx, st, o, root, lang, cfg.ExtractorsFor(lang), lc, cl)
			res.Notes = append(res.Notes, es.Notes...)
			if err != nil {
				res.Notes = append(res.Notes, fmt.Sprintf("%s: edges not computed: %v", lang, err))
				_ = st.SetMeta(ctx, edgesMetaKey(lang), "")
			} else {
				res.Edges += es.Edges
				res.Aliases += es.Aliases
				res.EdgeLangs++
				edgesRan = true
				o.logf("  %s: %d edge(s), %d alias(es)", lang, es.Edges, es.Aliases)
				_ = st.SetMeta(ctx, edgesMetaKey(lang), "1")
			}
		} else if edgeLangs[lang] {
			_ = st.SetMeta(ctx, edgesMetaKey(lang), "")
		}
		if cl != nil {
			cl.Close()
		}
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

func orDash(s string) string {
	if s == "" {
		return "server"
	}
	return s
}

func startServer(ctx context.Context, o Options, lc config.Language, root string) (*lsp.Client, error) {
	spec := lsp.Spec{
		Command: lc.Command, Args: lc.Args, Dir: root,
		StartupTimeout: time.Duration(lc.StartupTimeoutS) * time.Second,
		RequestTimeout: time.Duration(lc.RequestTimeoutS) * time.Second,
	}
	for _, k := range sortedEnv(lc.Env) {
		spec.Env = append(spec.Env, k+"="+lc.Env[k])
	}
	open := o.Open
	if open == nil {
		open = lsp.Start
	}
	cl, err := open(spec)
	if err != nil {
		return nil, err
	}
	if err := cl.Initialize(ctx, root); err != nil {
		cl.Close()
		return nil, err
	}
	if !cl.Capabilities().DocumentSymbol {
		cl.Close()
		return nil, fmt.Errorf("%s does not advertise documentSymbol", lc.Command)
	}
	return cl, nil
}

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

// DefaultEmptyRetryDelays are the waits before re-asking for symbols when a server
// answered with an empty list for a non-empty file. bash-language-server
// analyzes a document asynchronously after didOpen and answers [] (no error)
// until it is done, so an empty answer is not trusted at once.
var DefaultEmptyRetryDelays = []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second}

// retryState adapts the empty-answer retry to the server: once a retry has
// produced symbols the server is known to be asynchronous and retries stay on;
// after three retries that found nothing they stop (the files really are empty
// of symbols), so a server that answers promptly is not slowed down.
type retryState struct {
	helped, wasted int
	delays         []time.Duration
}

func (r *retryState) allow() bool { return r.helped > 0 || r.wasted < 3 }

func documentSymbols(ctx context.Context, cl *lsp.Client, uri, text string, rs *retryState) ([]lsp.Symbol, error) {
	syms, err := cl.DocumentSymbols(ctx, uri)
	if err != nil || len(syms) > 0 || strings.TrimSpace(text) == "" || !rs.allow() {
		return syms, err
	}
	for _, d := range rs.delays {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(d):
		}
		syms, err = cl.DocumentSymbols(ctx, uri)
		if err != nil || len(syms) > 0 {
			break
		}
	}
	if len(syms) > 0 {
		rs.helped++
	} else if err == nil {
		rs.wasted++
	}
	return syms, err
}

// indexFile extracts one file through cl and replaces its rows. When the
// server fails on the file, the file is recorded with status 'failed' (and no
// symbols) so the next run retries it, and the error is returned.
func indexFile(ctx context.Context, st *store.Store, cl *lsp.Client, rs *retryState, lc config.Language, c candidate, data []byte, sha string, oldID int64, hasVec bool) (fileStats, error) {
	text := strings.ToValidUTF8(string(data), "\ufffd")
	text = strings.TrimPrefix(text, "\ufeff") // editors send the text without a BOM
	lines := splitLines(text)
	uri := lsp.PathToURI(c.abs)

	if err := cl.DidOpen(uri, lc.LanguageID, 1, text); err != nil {
		return fileStats{}, err
	}
	defer cl.DidClose(uri)
	raw, err := documentSymbols(ctx, cl, uri, text, rs)
	if err != nil {
		_ = recordFailure(ctx, st, c, sha, oldID, hasVec, err)
		return fileStats{}, err
	}
	normalizeNames(raw, lines)
	syms := flatten(raw)
	for i := range syms {
		s := &syms[i]
		locateName(s, lines)
		s.BodySHA = bodySHA(lines, s.Range.Start.Line, s.Range.End.Line)
		s.Signature = s.Detail
		if hoverKinds[s.Kind] && cl.Capabilities().Hover {
			h, herr := cl.Hover(ctx, uri, s.SelectionRange.Start)
			if herr != nil {
				if errors.Is(herr, lsp.ErrClosed) {
					return fileStats{}, herr
				}
				continue // hover is best effort
			}
			if sig, doc := lsp.ParseHover(h); sig != "" || doc != "" {
				if sig != "" {
					s.Signature = sig
				}
				s.Doc = doc
			}
		}
		if hoverKinds[s.Kind] {
			// Fallbacks from the source when hover had nothing at the definition.
			if s.Doc == "" {
				s.Doc = leadingComment(lines, s.Range.Start.Line)
			}
			if s.Doc == "" {
				s.Doc = helpComment(lines, s.Range.Start.Line, s.Range.End.Line)
			}
			if s.Signature == "" {
				s.Signature = declarationLine(lines, s.Range.Start.Line)
			}
		}
	}
	return writeFile(ctx, st, c, sha, oldID, hasVec, syms)
}

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

func sortedEnv(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
