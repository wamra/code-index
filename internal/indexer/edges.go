package indexer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wamra/code-index/internal/config"
	"github.com/wamra/code-index/internal/extract"
	"github.com/wamra/code-index/internal/lsp"
	"github.com/wamra/code-index/internal/store"
)

// Edge kinds, sources and confidences as stored in the edges table.
const (
	EdgeCalls      = "calls"
	EdgeAliasOf    = "alias_of"
	EdgeImports    = "imports"
	EdgeDotSources = "dot_sources"
	EdgeInvokes    = "invokes" // a workspace file executed as a child process

	ConfExact      = extract.ResExact
	ConfAlias      = extract.ResAlias
	ConfCandidate  = extract.ResCandidate
	ConfDynamic    = extract.ResDynamic
	ConfUnresolved = extract.ResUnresolved
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
	reason    string // why the extractor chose the edge; empty when it said nothing
	hasTo     bool   // the record named the target file ("to"): authoritative for its call site
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
	exts      []string // the language's extensions, in config order (resolveFile)
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
//
// Rows with the same key (kind, caller, callee, call site) merge: the source
// becomes lsp+ast when both found it. Confidence and reason of the merged row:
//   - when exactly one of the two edges carried "to" (hasTo), that edge decides
//     both: it is the extractor's statement about the exact destination;
//   - otherwise the stronger confidence wins (exact > alias > candidate), with
//     the reason of the edge that supplied it; on a tie the record's (ast)
//     reason is kept when it has one, else the first non-empty reason.
//
// An edge with hasTo is also authoritative for its call site (file, line,
// col): sorted drops every LSP-only calls edge at that site whose destination
// is not one of the destinations the records named there. The result does not
// depend on the order of add calls.
type edgeSet struct{ m map[string]*edgeRow }

func (s *edgeSet) add(e edgeRow) {
	if s.m == nil {
		s.m = map[string]*edgeRow{}
	}
	k := e.key()
	old, ok := s.m[k]
	if !ok {
		s.m[k] = &e
		return
	}
	old.lsp = old.lsp || e.lsp
	old.ast = old.ast || e.ast
	if e.ast && e.dstName != "" {
		old.dstName = e.dstName
	}
	switch {
	case e.hasTo && !old.hasTo:
		old.conf, old.reason, old.hasTo = e.conf, e.reason, true
	case old.hasTo && !e.hasTo:
		// the earlier record edge keeps its confidence and reason
	default:
		oc, ec := extract.Strength(old.conf), extract.Strength(e.conf)
		switch {
		case ec > oc:
			old.conf, old.reason = e.conf, e.reason
		case ec == oc && e.ast && e.reason != "":
			old.reason = e.reason
		case ec == oc && old.reason == "":
			old.reason = e.reason
		}
	}
}

// siteKey identifies a call site.
type siteKey struct {
	file      int64
	line, col int
}

func (s *edgeSet) sorted() []*edgeRow {
	named := map[siteKey]map[int64]bool{} // call sites that records gave a target file for -> destinations
	for _, e := range s.m {
		if e.hasTo && e.kind == EdgeCalls {
			k := siteKey{e.file, e.line, e.col}
			if named[k] == nil {
				named[k] = map[int64]bool{}
			}
			named[k][e.dst] = true
		}
	}
	out := make([]*edgeRow, 0, len(s.m))
	for _, e := range s.m {
		if dsts := named[siteKey{e.file, e.line, e.col}]; dsts != nil && e.kind == EdgeCalls && e.lsp && !e.ast && !dsts[e.dst] {
			continue
		}
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

// sourced is a record together with the kind of extractor that wrote it: edges
// from an lsp extractor are stored with source lsp, edges from an exec
// extractor with source ast (so lsp+ast means both agree).
type sourced struct {
	extract.Record
	lsp bool
}

// computeEdges rebuilds every edge whose call site is in a file of lang: the
// records of the language's sessions (live, in run order; each one that
// provides edges is asked for all the language's files its extensions select)
// merged by edgeSet. The rebuild covers all files of the language, not only the
// changed ones: a change in one file moves symbol ids and changes who calls
// whom in the others. exts are the language's extensions (resolveFile).
func computeEdges(ctx context.Context, st *store.Store, o Options, root, lang string, live []*liveSession, exts []string) (edgeStats, error) {
	var es edgeStats
	idx, err := loadIndex(ctx, st.DB(), root, lang)
	if err != nil {
		return es, err
	}
	idx.exts = exts
	set := &edgeSet{}
	var recs []sourced
	var ran []string
	var known []extract.Record
	haveKnown := false
	for _, ls := range live {
		if !ls.prov.Edges {
			if ls.ne.Type == config.ExtractorLSP {
				es.Notes = append(es.Notes, fmt.Sprintf("%s: extractor %s does not advertise references or call hierarchy; no LSP edges", lang, ls.ne.Name))
			}
			continue
		}
		files := filesWithExtensions(idx.files, ls.ne.Extensions)
		if len(files) == 0 {
			continue
		}
		if !haveKnown {
			if known, err = knownSymbols(ctx, st.DB(), root, lang); err != nil {
				return es, err
			}
			haveKnown = true
		}
		var got []extract.Record
		err := ls.sess.Edges(ctx, files, known, func(_ string, rs []extract.Record) error {
			got = append(got, rs...)
			return nil
		})
		if n, ok := ls.sess.(extract.Noter); ok {
			for _, line := range n.Notes() {
				es.Notes = append(es.Notes, lang+": "+line)
			}
		}
		switch {
		case errors.Is(err, ErrExtractorUnavailable):
			es.Notes = append(es.Notes, fmt.Sprintf("%s: %v; skipping the AST extractor %s", lang, err, ls.ne.Name))
			continue
		case err != nil:
			return es, fmt.Errorf("extractor %s: %w", ls.ne.Name, err)
		}
		ran = append(ran, ls.ne.Name)
		for _, r := range got {
			if r.T == extract.TypeError {
				es.Notes = append(es.Notes, fmt.Sprintf("%s: extractor failed on %s: %s", lang, r.File, r.Message))
				continue
			}
			recs = append(recs, sourced{r, ls.ne.Type == config.ExtractorLSP})
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
	ar, err := astEdges(ctx, tx, idx, recs, set)
	if err != nil {
		return es, err
	}
	es.Aliases = ar.aliases
	if ar.noResolution > 0 {
		es.Notes = append(es.Notes, fmt.Sprintf("%s: ignored %d call record(s) without a resolution (extractor %s writes schema v1)", lang, ar.noResolution, strings.Join(ran, ",")))
	}
	if ar.notIndexed > 0 {
		es.Notes = append(es.Notes, fmt.Sprintf("%s: %d call record(s) point to a definition that is not indexed", lang, ar.notIndexed))
	}
	for _, e := range set.sorted() {
		var src, dst any
		if e.src != 0 {
			src = e.src
		}
		if e.dst != 0 {
			dst = e.dst
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO edges(src_symbol_id, dst_symbol_id, dst_name, kind, file_id, line, col, source, confidence, reason)
			VALUES (?,?,?,?,?,?,?,?,?,?)`, src, dst, nullIf(e.dstName), e.kind, e.file, e.line, e.col, e.sourceName(), e.conf, nullIf(e.reason)); err != nil {
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

// astResult is what astEdges counted besides the edges it added.
type astResult struct {
	aliases      int // alias symbols created
	noResolution int // call records rejected for lacking a valid resolution
	notIndexed   int // call records whose "to" file or symbol is not indexed
}

// astEdges turns the extractor records into alias symbols (inserted in tx)
// and edges (added to set); docs/extractors.md says how each record type is
// read.
func astEdges(ctx context.Context, tx *sql.Tx, idx *symIndex, recs []sourced, set *edgeSet) (astResult, error) {
	var res astResult
	aliasTarget := map[string]string{} // lower alias -> target text
	aliasID := map[string]int64{}
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
		ins, err := tx.ExecContext(ctx, `INSERT INTO symbols(file_id, name, qualified_name, kind, parent_id,
			start_line, start_col, end_line, end_col, sel_line, sel_col, signature, doc, body_sha256, source)
			VALUES (?,?,?,?,NULL,?,?,?,?,?,?,?,NULL,?, 'ast')`,
			f.id, r.Alias, r.Alias, "alias", line, col, endLine, endCol, line, col, sig, sha256Hex([]byte(sig)))
		if err != nil {
			return res, err
		}
		id, _ := ins.LastInsertId()
		if _, err := tx.ExecContext(ctx, `INSERT INTO name_fts(rowid, name, qualified_name, path) VALUES (?,?,?,?)`, id, r.Alias, r.Alias, f.path); err != nil {
			return res, err
		}
		if err := addChunk(ctx, tx, "symbol", id, f.id, SymbolChunk("alias", r.Alias, f.path, sig, "")); err != nil {
			return res, err
		}
		res.aliases++
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
	srcOf := func(r extract.Record, f *fileInfo) int64 {
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
		e := edgeRow{file: f.id, line: max(r.Line-1, 0), col: max(r.Col-1, 0), lsp: r.lsp, ast: !r.lsp, reason: r.Reason}
		rres, valid := extract.NormalizeResolution(r.T, r.Resolution, r.Reason)
		switch r.T {
		case extract.TypeAlias:
			id, ok := aliasID[strings.ToLower(r.Alias)]
			if !ok {
				continue
			}
			e.kind, e.src, e.dstName, e.conf = EdgeAliasOf, id, r.Target, ConfUnresolved
			if t := resolve(r.Target, f.id); t != nil {
				e.dst, e.conf = t.id, ConfExact
			}
		case extract.TypeCall:
			if r.Name == "" {
				continue
			}
			if !valid {
				res.noResolution++
				continue
			}
			e.kind, e.src, e.dstName = EdgeCalls, srcOf(r.Record, f), r.Name
			if r.To != "" {
				// The record names the file that holds the definition: look there, never by name alone.
				tf := idx.fileForAbs(r.To)
				var t *symLoc
				if tf != nil {
					t = idx.symbolIn(tf.id, r.Name, r.ToLine, r.ToCol)
				}
				if t == nil {
					res.notIndexed++
					continue
				}
				e.dst, e.conf, e.hasTo = t.id, rres, !r.lsp // an lsp edge is evidence, not the authority on its site
				break
			}
			// Name only. The alias table and pickFunc decide as they always did;
			// an extractor that is not sure marks the edge candidate.
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
			if rres == ConfCandidate && (e.conf == ConfExact || e.conf == ConfAlias) {
				e.conf = ConfCandidate
			}
		case extract.TypeDynamic:
			e.kind, e.src, e.dstName, e.conf = EdgeCalls, srcOf(r.Record, f), r.Expr, ConfDynamic
		case extract.TypeDotSource, extract.TypeImport:
			if rres == ConfUnresolved {
				continue
			}
			var target *fileInfo
			if r.T == extract.TypeDotSource {
				e.kind = EdgeDotSources
				target = idx.resolveFile(f, r.Path, false)
			} else {
				e.kind = EdgeImports
				target = idx.resolveFile(f, r.Path, true)
			}
			if target == nil {
				continue
			}
			e.src, e.dstName, e.conf = srcOf(r.Record, f), target.path, ConfExact
		case extract.TypeInvoke:
			if rres == ConfUnresolved {
				continue
			}
			target := idx.fileForAbs(r.Path)
			if r.Path == "" || target == nil {
				continue
			}
			e.kind, e.src, e.dstName, e.conf = EdgeInvokes, srcOf(r.Record, f), target.path, rres
		default:
			continue
		}
		set.add(e)
	}
	return res, nil
}

// symbolIn returns the callable symbol called name in the file: with a 1-based
// position (line > 0), the one whose name is at that position; else, or when
// none is there, the exact-case match, then the case-insensitive match, the
// first by position. nil when the file has no such symbol.
func (x *symIndex) symbolIn(fileID int64, name string, line, col int) *symLoc {
	var exact, folded *symLoc
	for _, s := range x.byFile[fileID] {
		if !callableKinds[s.kind] || !strings.EqualFold(s.name, name) {
			continue
		}
		if line > 0 && s.selLine == line-1 && s.selCol == max(col-1, 0) {
			return s
		}
		if folded == nil {
			folded = s
		}
		if exact == nil && s.name == name {
			exact = s
		}
	}
	if exact != nil {
		return exact
	}
	return folded
}

// resolveFile finds the indexed file a dot-sourced or imported path names. The
// literal is relative to the importing file, else to the repo root. A literal
// that ends in one of the language's extensions is a file path. An import
// argument that is a bare name matches an indexed file by base name plus one of
// the language's extensions, the later-listed extension first (a manifest
// extension listed last wins over a script extension listed first).
func (x *symIndex) resolveFile(from *fileInfo, lit string, module bool) *fileInfo {
	lit = strings.Trim(strings.ReplaceAll(strings.TrimSpace(lit), `\`, "/"), `"'`)
	if lit == "" {
		return nil
	}
	for _, e := range x.exts {
		if strings.EqualFold(path.Ext(lit), e) {
			module = false
		}
	}
	if module && !strings.Contains(lit, "/") {
		for i := len(x.exts) - 1; i >= 0; i-- {
			ext := x.exts[i]
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

// filesWithExtensions returns the absolute paths of the files whose extension
// (case-insensitive) is in exts.
func filesWithExtensions(files []*fileInfo, exts []string) []string {
	want := make(map[string]bool, len(exts))
	for _, e := range exts {
		want[strings.ToLower(e)] = true
	}
	var out []string
	for _, f := range files {
		if want[strings.ToLower(filepath.Ext(f.path))] {
			out = append(out, f.abs)
		}
	}
	return out
}
