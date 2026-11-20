package indexer

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf16"

	"github.com/wamra/code-index/internal/config"
	"github.com/wamra/code-index/internal/lsp"
	"github.com/wamra/code-index/internal/store"
)

// Edge kinds, sources and confidences as stored in the edges table.
const (
	EdgeCalls      = "calls"
	EdgeAliasOf    = "alias_of"
	EdgeImports    = "imports"
	EdgeDotSources = "dot_sources"

	ConfExact      = "exact"
	ConfAlias      = "alias"
	ConfDynamic    = "dynamic"
	ConfUnresolved = "unresolved"
)

// edgeRow is an edge before it is written. src and dst are symbol ids; src 0
// means the top level of the file, dst 0 means no workspace symbol (dynamic
// calls, unresolved alias targets, files).
type edgeRow struct {
	src, dst  int64
	dstName   string
	kind      string
	file      int64
	line, col int // zero-based, like the symbols table
	lsp, ast  bool
	conf      string
}

func (e *edgeRow) key() string {
	k := fmt.Sprintf("%s|%d|%d|%d|%d|%d", e.kind, e.src, e.dst, e.file, e.line, e.col)
	if e.dst == 0 {
		k += "|" + e.dstName
	}
	return k
}

func (e *edgeRow) sourceName() string {
	switch {
	case e.lsp && e.ast:
		return "lsp+ast"
	case e.lsp:
		return "lsp"
	}
	return "ast"
}

type fileInfo struct {
	id   int64
	path string
	abs  string
}

type symLoc struct {
	id, fileID       int64
	name, qual, kind string
	sl, sc, el, ec   int
	selLine, selCol  int
}

// symIndex is the in-memory view of one language's files and symbols that
// edge extraction maps positions and names through.
type symIndex struct {
	root      string
	files     []*fileInfo
	byPath    map[string]*fileInfo
	byPathLow map[string]*fileInfo
	byFile    map[int64][]*symLoc
	funcs     map[string][]*symLoc // lower-case name -> function-like symbols
	fileByID  map[int64]*fileInfo
}

// callable kinds get a references request; container kinds can be the source
// of an edge (variables are not: PowerShell servers list $x = ... as symbols).
var (
	callableKinds  = map[string]bool{"function": true, "method": true, "constructor": true}
	containerKinds = map[string]bool{"function": true, "method": true, "constructor": true, "class": true, "struct": true, "interface": true}
)

func loadIndex(ctx context.Context, db *sql.DB, root, lang string) (*symIndex, error) {
	x := &symIndex{root: root, byPath: map[string]*fileInfo{}, byPathLow: map[string]*fileInfo{},
		byFile: map[int64][]*symLoc{}, funcs: map[string][]*symLoc{}, fileByID: map[int64]*fileInfo{}}
	rows, err := db.QueryContext(ctx, `SELECT id, path FROM files WHERE lang=? AND status='ok' ORDER BY path`, lang)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		f := &fileInfo{}
		if err := rows.Scan(&f.id, &f.path); err != nil {
			rows.Close()
			return nil, err
		}
		f.abs = filepath.Join(root, filepath.FromSlash(f.path))
		x.files = append(x.files, f)
		x.byPath[f.path] = f
		x.byPathLow[strings.ToLower(f.path)] = f
		x.fileByID[f.id] = f
	}
	rows.Close()
	rows, err = db.QueryContext(ctx, `SELECT s.id, s.file_id, s.name, s.qualified_name, s.kind, s.start_line, s.start_col, s.end_line, s.end_col,
		COALESCE(s.sel_line, s.start_line), COALESCE(s.sel_col, s.start_col)
		FROM symbols s JOIN files f ON f.id = s.file_id WHERE f.lang=? AND f.status='ok' ORDER BY f.path, s.start_line, s.start_col, s.id`, lang)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		s := &symLoc{}
		if err := rows.Scan(&s.id, &s.fileID, &s.name, &s.qual, &s.kind, &s.sl, &s.sc, &s.el, &s.ec, &s.selLine, &s.selCol); err != nil {
			return nil, err
		}
		x.byFile[s.fileID] = append(x.byFile[s.fileID], s)
		if callableKinds[s.kind] {
			k := strings.ToLower(s.name)
			x.funcs[k] = append(x.funcs[k], s)
		}
	}
	return x, rows.Err()
}

func (x *symIndex) fileForAbs(abs string) *fileInfo {
	rel, err := filepath.Rel(x.root, abs)
	if err != nil {
		return nil
	}
	r := filepath.ToSlash(rel)
	if r == ".." || strings.HasPrefix(r, "../") {
		return nil
	}
	if f := x.byPath[r]; f != nil {
		return f
	}
	return x.byPathLow[strings.ToLower(r)]
}

func (x *symIndex) fileForURI(uri string) *fileInfo {
	p, err := lsp.URIToPath(uri)
	if err != nil {
		return nil
	}
	return x.fileForAbs(p)
}

// container returns the innermost container symbol of the file that covers
// (line, col), or nil for the top level.
func (x *symIndex) container(fileID int64, line, col int) *symLoc {
	var best *symLoc
	bestSpan := 0
	for _, s := range x.byFile[fileID] {
		if !containerKinds[s.kind] {
			continue
		}
		if line < s.sl || line > s.el || (line == s.sl && col < s.sc) || (line == s.el && col > s.ec) {
			continue
		}
		span := (s.el-s.sl)*100000 + (s.ec - s.sc)
		if best == nil || span < bestSpan {
			best, bestSpan = s, span
		}
	}
	return best
}

// pickFunc chooses among functions of one name: the one in the calling file,
// else the first by path.
func pickFunc(c []*symLoc, fromFile int64) *symLoc {
	for _, s := range c {
		if s.fileID == fromFile {
			return s
		}
	}
	if len(c) > 0 {
		return c[0]
	}
	return nil
}

// edgeSet merges edges found by more than one source into one row.
type edgeSet struct{ m map[string]*edgeRow }

func (s *edgeSet) add(e edgeRow) {
	if s.m == nil {
		s.m = map[string]*edgeRow{}
	}
	k := e.key()
	if old, ok := s.m[k]; ok {
		old.lsp = old.lsp || e.lsp
		old.ast = old.ast || e.ast
		if e.ast && e.dstName != "" {
			old.dstName = e.dstName
		}
		if old.lsp {
			old.conf = ConfExact
		}
		return
	}
	s.m[k] = &e
}

func (s *edgeSet) sorted() []*edgeRow {
	out := make([]*edgeRow, 0, len(s.m))
	for _, e := range s.m {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.file != b.file {
			return a.file < b.file
		}
		if a.line != b.line {
			return a.line < b.line
		}
		if a.col != b.col {
			return a.col < b.col
		}
		return a.key() < b.key()
	})
	return out
}

type edgeStats struct {
	Edges, Aliases int
	Notes          []string
}

// computeEdges rebuilds every edge whose call site is in a file of lang: LSP
// references (cl, when the server advertises them) merged with the AST
// extractor's output (when the language configures one). The rebuild covers
// all files of the language, not only the changed ones: a change in one file
// moves symbol ids and changes who references whom in the others.
func computeEdges(ctx context.Context, st *store.Store, o Options, root, lang string, lc config.Language, cl *lsp.Client) (edgeStats, error) {
	var es edgeStats
	idx, err := loadIndex(ctx, st.DB(), root, lang)
	if err != nil {
		return es, err
	}
	set := &edgeSet{}
	if cl != nil {
		if cl.Capabilities().References {
			reqs, failed, first, err := lspEdges(ctx, cl, lc, idx, set)
			if err != nil {
				return es, err
			}
			o.logf("  %s: %d references request(s)", lang, reqs)
			if failed > 0 {
				es.Notes = append(es.Notes, fmt.Sprintf("%s: %d of %d references request(s) failed (first: %v)", lang, failed, reqs, first))
			}
		} else {
			es.Notes = append(es.Notes, fmt.Sprintf("%s: the server does not advertise references; no LSP edges", lang))
		}
	}
	var recs []astRec
	if lc.Extractor != "" {
		var files []string
		for _, f := range idx.files {
			if ext := strings.ToLower(filepath.Ext(f.path)); ext == ".ps1" || ext == ".psm1" {
				files = append(files, f.abs)
			}
		}
		if len(files) > 0 {
			extract := o.Extract
			if extract == nil {
				extract = defaultExtract
			}
			out, err := extract(ctx, lc.Extractor, files)
			switch {
			case errors.Is(err, ErrExtractorUnavailable):
				es.Notes = append(es.Notes, fmt.Sprintf("%s: %v; skipping the AST extractor", lang, err))
			case err != nil:
				return es, fmt.Errorf("extractor %s: %w", lc.Extractor, err)
			default:
				var skipped int
				recs, skipped, err = parseASTLines(bytes.NewReader(out))
				if err != nil {
					return es, err
				}
				if skipped > 0 {
					es.Notes = append(es.Notes, fmt.Sprintf("%s: ignored %d non-JSON line(s) of extractor output", lang, skipped))
				}
				for _, r := range recs {
					if r.T == "error" {
						es.Notes = append(es.Notes, fmt.Sprintf("%s: extractor failed on %s: %s", lang, r.File, r.Message))
					}
				}
			}
		}
	}

	hasVec := false
	if d, err := st.VecDims(ctx); err == nil && d > 0 {
		hasVec = true
	}
	tx, err := st.DB().BeginTx(ctx, nil)
	if err != nil {
		return es, err
	}
	defer tx.Rollback()
	if err := deleteLangEdges(ctx, tx, lang, hasVec); err != nil {
		return es, err
	}
	n, err := astEdges(ctx, tx, idx, recs, set)
	if err != nil {
		return es, err
	}
	es.Aliases = n
	for _, e := range set.sorted() {
		var src, dst any
		if e.src != 0 {
			src = e.src
		}
		if e.dst != 0 {
			dst = e.dst
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO edges(src_symbol_id, dst_symbol_id, dst_name, kind, file_id, line, col, source, confidence)
			VALUES (?,?,?,?,?,?,?,?,?)`, src, dst, nullIf(e.dstName), e.kind, e.file, e.line, e.col, e.sourceName(), e.conf); err != nil {
			return es, err
		}
		es.Edges++
	}
	return es, tx.Commit()
}

// deleteLangEdges removes the edges called from files of lang and the symbols
// the AST extractor created there (aliases), with their FTS, chunk and vector rows.
func deleteLangEdges(ctx context.Context, tx *sql.Tx, lang string, hasVec bool) error {
	const astSyms = `SELECT s.id FROM symbols s JOIN files f ON f.id = s.file_id WHERE f.lang=?1 AND s.source='ast'`
	stmts := []string{
		`DELETE FROM edges WHERE file_id IN (SELECT id FROM files WHERE lang=?1)`,
		`DELETE FROM name_fts WHERE rowid IN (` + astSyms + `)`,
		`DELETE FROM chunk_fts WHERE rowid IN (SELECT id FROM chunks WHERE target_kind='symbol' AND target_id IN (` + astSyms + `))`,
	}
	if hasVec {
		stmts = append(stmts, `DELETE FROM chunk_vec WHERE rowid IN (SELECT id FROM chunks WHERE target_kind='symbol' AND target_id IN (`+astSyms+`))`)
	}
	stmts = append(stmts,
		`DELETE FROM chunks WHERE target_kind='symbol' AND target_id IN (`+astSyms+`)`,
		`DELETE FROM symbols WHERE id IN (`+astSyms+`)`,
	)
	for _, q := range stmts {
		if _, err := tx.ExecContext(ctx, q, lang); err != nil {
			return err
		}
	}
	return nil
}

// lspEdges asks the server for the references of every function-like symbol
// and records each as a calls edge from the innermost container at the
// reference. A failing request is counted and skipped; a dead server is fatal.
func lspEdges(ctx context.Context, cl *lsp.Client, lc config.Language, idx *symIndex, set *edgeSet) (reqs, failed int, first error, fatal error) {
	for _, f := range idx.files {
		var targets []*symLoc
		for _, s := range idx.byFile[f.id] {
			if callableKinds[s.kind] {
				targets = append(targets, s)
			}
		}
		if len(targets) == 0 {
			continue
		}
		data, err := os.ReadFile(f.abs)
		if err != nil {
			continue
		}
		text := strings.TrimPrefix(strings.ToValidUTF8(string(data), "\ufffd"), "\ufeff")
		uri := lsp.PathToURI(f.abs)
		if err := cl.DidOpen(uri, lc.LanguageID, 1, text); err != nil {
			return reqs, failed, first, err
		}
		for _, t := range targets {
			if err := ctx.Err(); err != nil {
				cl.DidClose(uri)
				return reqs, failed, first, err
			}
			reqs++
			locs, err := cl.References(ctx, uri, lsp.Position{Line: t.selLine, Character: t.selCol})
			if err != nil {
				if errors.Is(err, lsp.ErrClosed) {
					return reqs, failed, first, err
				}
				failed++
				if first == nil {
					first = err
				}
				continue
			}
			selEnd := t.selCol + len(utf16.Encode([]rune(t.name)))
			for _, l := range locs {
				rf := idx.fileForURI(l.URI)
				if rf == nil {
					continue
				}
				p := l.Range.Start
				if rf.id == f.id && p.Line == t.selLine && p.Character >= t.selCol && p.Character < selEnd {
					continue // the declaration itself
				}
				e := edgeRow{dst: t.id, kind: EdgeCalls, file: rf.id, line: p.Line, col: p.Character, lsp: true, conf: ConfExact}
				if c := idx.container(rf.id, p.Line, p.Character); c != nil {
					e.src = c.id
				}
				set.add(e)
			}
		}
		cl.DidClose(uri)
	}
	return reqs, failed, first, nil
}

// astEdges turns the extractor records into alias symbols (inserted in tx)
// and edges (added to set). It returns the number of alias symbols created.
func astEdges(ctx context.Context, tx *sql.Tx, idx *symIndex, recs []astRec, set *edgeSet) (int, error) {
	aliasTarget := map[string]string{} // lower alias -> target text
	aliasID := map[string]int64{}
	aliases := 0
	for _, r := range recs {
		if r.T != "alias" || r.Alias == "" || r.Target == "" {
			continue
		}
		f := idx.fileForAbs(r.File)
		if f == nil {
			continue
		}
		line, col := max(r.Line-1, 0), max(r.Col-1, 0)
		endLine, endCol := max(r.EndLine-1, line), max(r.EndCol-1, col)
		sig := r.Alias + " -> " + r.Target
		res, err := tx.ExecContext(ctx, `INSERT INTO symbols(file_id, name, qualified_name, kind, parent_id,
			start_line, start_col, end_line, end_col, sel_line, sel_col, signature, doc, body_sha256, source)
			VALUES (?,?,?,?,NULL,?,?,?,?,?,?,?,NULL,?, 'ast')`,
			f.id, r.Alias, r.Alias, "alias", line, col, endLine, endCol, line, col, sig, sha256Hex([]byte(sig)))
		if err != nil {
			return 0, err
		}
		id, _ := res.LastInsertId()
		if _, err := tx.ExecContext(ctx, `INSERT INTO name_fts(rowid, name, qualified_name, path) VALUES (?,?,?,?)`, id, r.Alias, r.Alias, f.path); err != nil {
			return 0, err
		}
		if err := addChunk(ctx, tx, "symbol", id, f.id, SymbolChunk("alias", r.Alias, f.path, sig, "")); err != nil {
			return 0, err
		}
		aliases++
		k := strings.ToLower(r.Alias)
		if _, dup := aliasTarget[k]; !dup {
			aliasTarget[k] = r.Target
			aliasID[k] = id
		}
		idx.byFile[f.id] = append(idx.byFile[f.id], &symLoc{id: id, fileID: f.id, name: r.Alias, qual: r.Alias, kind: "alias", sl: line, sc: col, el: endLine, ec: endCol})
	}

	// resolve follows alias chains (an alias of an alias) to a function.
	resolve := func(name string, from int64) *symLoc {
		for i := 0; i < 5; i++ {
			t, ok := aliasTarget[strings.ToLower(name)]
			if !ok {
				break
			}
			name = t
		}
		return pickFunc(idx.funcs[strings.ToLower(name)], from)
	}
	srcOf := func(r astRec, f *fileInfo) int64 {
		if c := idx.container(f.id, r.Line-1, r.Col-1); c != nil {
			return c.id
		}
		if r.In != "" {
			for _, s := range idx.byFile[f.id] {
				if containerKinds[s.kind] && strings.EqualFold(s.name, r.In) {
					return s.id
				}
			}
		}
		return 0
	}

	for _, r := range recs {
		f := idx.fileForAbs(r.File)
		if f == nil {
			continue
		}
		e := edgeRow{file: f.id, line: max(r.Line-1, 0), col: max(r.Col-1, 0), ast: true}
		switch r.T {
		case "alias":
			id, ok := aliasID[strings.ToLower(r.Alias)]
			if !ok {
				continue
			}
			e.kind, e.src, e.dstName, e.conf = EdgeAliasOf, id, r.Target, ConfUnresolved
			if t := resolve(r.Target, f.id); t != nil {
				e.dst, e.conf = t.id, ConfExact
			}
		case "call":
			if r.Name == "" {
				continue
			}
			e.kind, e.src, e.dstName = EdgeCalls, srcOf(r, f), r.Name
			if _, isAlias := aliasTarget[strings.ToLower(r.Name)]; isAlias {
				if t := resolve(r.Name, f.id); t != nil {
					e.dst, e.conf = t.id, ConfAlias
				} else {
					e.dst, e.conf = aliasID[strings.ToLower(r.Name)], ConfUnresolved
				}
			} else if t := pickFunc(idx.funcs[strings.ToLower(r.Name)], f.id); t != nil {
				e.dst, e.conf = t.id, ConfExact
			} else {
				continue // a built-in or external command: not part of the workspace graph
			}
		case "dynamic":
			e.kind, e.src, e.dstName, e.conf = EdgeCalls, srcOf(r, f), r.Expr, ConfDynamic
		case "dotsource", "import":
			var target *fileInfo
			if r.T == "dotsource" {
				e.kind = EdgeDotSources
				target = idx.resolveFile(f, r.Path, false)
			} else {
				e.kind = EdgeImports
				target = idx.resolveFile(f, r.Path, true)
			}
			if target == nil {
				continue
			}
			e.src, e.dstName, e.conf = srcOf(r, f), target.path, ConfExact
		default:
			continue
		}
		set.add(e)
	}
	return aliases, nil
}

// resolveFile finds the indexed file a dot-sourced or imported path names. The
// literal is relative to the importing file, else to the repo root; an
// Import-Module argument that is a bare name matches a module file by name
// (.psd1 before .psm1).
func (x *symIndex) resolveFile(from *fileInfo, lit string, module bool) *fileInfo {
	lit = strings.Trim(strings.ReplaceAll(strings.TrimSpace(lit), `\`, "/"), `"'`)
	if lit == "" {
		return nil
	}
	switch strings.ToLower(path.Ext(lit)) {
	case ".ps1", ".psm1", ".psd1":
		module = false
	}
	if module && !strings.Contains(lit, "/") {
		for _, ext := range []string{".psd1", ".psm1", ".ps1"} {
			for _, f := range x.files {
				if strings.EqualFold(path.Base(f.path), lit+ext) {
					return f
				}
			}
		}
		return nil
	}
	var cands []string
	if filepath.IsAbs(filepath.FromSlash(lit)) {
		if f := x.fileForAbs(filepath.FromSlash(lit)); f != nil {
			return f
		}
		return nil
	}
	cands = append(cands, path.Join(path.Dir(from.path), lit), path.Clean(lit))
	for _, c := range cands {
		if f := x.byPath[c]; f != nil {
			return f
		}
		if f := x.byPathLow[strings.ToLower(c)]; f != nil {
			return f
		}
	}
	return nil
}

const maxLinkNames = 10

// WithLinks appends the "calls:" and "called by:" lines of the chunk text.
func WithLinks(base string, calls, calledBy []string) string {
	var b strings.Builder
	b.WriteString(base)
	line := func(label string, names []string) {
		if len(names) == 0 {
			return
		}
		more := ""
		if len(names) > maxLinkNames {
			more = fmt.Sprintf(" (+%d more)", len(names)-maxLinkNames)
			names = names[:maxLinkNames]
		}
		b.WriteString("\n" + label + ": " + strings.Join(names, ", ") + more)
	}
	line("calls", calls)
	line("called by", calledBy)
	return b.String()
}

// refreshLinks rewrites the symbol chunks whose "calls:" / "called by:" lines
// changed with the current edges. Only chunks whose text changed are touched;
// they go back to pending so the next embedding run re-embeds exactly those.
func refreshLinks(ctx context.Context, st *store.Store) (int, error) {
	db := st.DB()
	calls, calledBy := map[int64]map[string]bool{}, map[int64]map[string]bool{}
	rows, err := db.QueryContext(ctx, `SELECT e.src_symbol_id, e.dst_symbol_id, COALESCE(ss.qualified_name, f.path), ds.qualified_name
		FROM edges e JOIN files f ON f.id = e.file_id
		LEFT JOIN symbols ss ON ss.id = e.src_symbol_id JOIN symbols ds ON ds.id = e.dst_symbol_id
		WHERE e.kind = 'calls'`)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var src sql.NullInt64
		var dst int64
		var srcName, dstName string
		if err := rows.Scan(&src, &dst, &srcName, &dstName); err != nil {
			rows.Close()
			return 0, err
		}
		if src.Valid {
			if calls[src.Int64] == nil {
				calls[src.Int64] = map[string]bool{}
			}
			calls[src.Int64][dstName] = true
		}
		if calledBy[dst] == nil {
			calledBy[dst] = map[string]bool{}
		}
		calledBy[dst][srcName] = true
	}
	rows.Close()
	names := func(m map[string]bool) []string {
		out := make([]string, 0, len(m))
		for n := range m {
			out = append(out, n)
		}
		sort.Strings(out)
		return out
	}

	type chunkRow struct {
		id, symID                 int64
		sha                       string
		kind, qual, sig, doc, rel string
	}
	rows, err = db.QueryContext(ctx, `SELECT c.id, c.text_sha256, s.id, s.kind, s.qualified_name, COALESCE(s.signature,''), COALESCE(s.doc,''), f.path
		FROM chunks c JOIN symbols s ON c.target_kind='symbol' AND s.id = c.target_id JOIN files f ON f.id = s.file_id`)
	if err != nil {
		return 0, err
	}
	var all []chunkRow
	for rows.Next() {
		var r chunkRow
		if err := rows.Scan(&r.id, &r.sha, &r.symID, &r.kind, &r.qual, &r.sig, &r.doc, &r.rel); err != nil {
			rows.Close()
			return 0, err
		}
		all = append(all, r)
	}
	rows.Close()

	hasVec := false
	if d, err := st.VecDims(ctx); err == nil && d > 0 {
		hasVec = true
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	changed := 0
	for _, r := range all {
		text := WithLinks(SymbolChunk(r.kind, r.qual, r.rel, r.sig, r.doc), names(calls[r.symID]), names(calledBy[r.symID]))
		sha := sha256Hex([]byte(text))
		if sha == r.sha {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE chunks SET text=?, text_sha256=?, embedded_at=NULL, embed_fingerprint=NULL WHERE id=?`, text, sha, r.id); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM chunk_fts WHERE rowid=?`, r.id); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO chunk_fts(rowid, text) VALUES (?,?)`, r.id, text); err != nil {
			return 0, err
		}
		if hasVec {
			if _, err := tx.ExecContext(ctx, `DELETE FROM chunk_vec WHERE rowid=?`, r.id); err != nil {
				return 0, err
			}
		}
		changed++
	}
	return changed, tx.Commit()
}
