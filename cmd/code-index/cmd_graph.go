package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"

	"github.com/wamra/code-index/internal/store"
)

// edgeOut is one call edge as printed. Line and Col are 1-based and point at
// the call site (Path is the file that contains it).
type edgeOut struct {
	Kind       string `json:"kind"`
	Path       string `json:"path"`
	Line       int    `json:"line"`
	Col        int    `json:"col"`
	From       string `json:"from"`      // caller qualified name; the file path for top-level code
	FromKind   string `json:"from_kind"` // symbol kind, or "file"
	To         string `json:"to"`        // callee qualified name; the expression text for dynamic calls
	ToKind     string `json:"to_kind"`   // symbol kind, or "" when no workspace symbol
	Via        string `json:"via,omitempty"`
	Source     string `json:"source"`
	Confidence string `json:"confidence"`
	Depth      int    `json:"depth,omitempty"`
	srcID      int64
	dstID      int64
}

const edgeSelect = `SELECT COALESCE(e.src_symbol_id,0), COALESCE(e.dst_symbol_id,0), COALESCE(e.dst_name,''), e.kind, f.path, e.line+1, e.col+1,
	e.source, e.confidence, COALESCE(ss.qualified_name,''), COALESCE(ss.kind,''), COALESCE(ds.qualified_name,''), COALESCE(ds.kind,'')
	FROM edges e JOIN files f ON f.id = e.file_id
	LEFT JOIN symbols ss ON ss.id = e.src_symbol_id LEFT JOIN symbols ds ON ds.id = e.dst_symbol_id`

func queryEdges(ctx context.Context, db *sql.DB, where string, args ...any) ([]edgeOut, error) {
	rows, err := db.QueryContext(ctx, edgeSelect+" "+where+" ORDER BY f.path, e.line, e.col, e.id", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []edgeOut
	for rows.Next() {
		var e edgeOut
		var dstName string
		if err := rows.Scan(&e.srcID, &e.dstID, &dstName, &e.Kind, &e.Path, &e.Line, &e.Col, &e.Source, &e.Confidence, &e.From, &e.FromKind, &e.To, &e.ToKind); err != nil {
			return nil, err
		}
		if e.srcID == 0 {
			e.From, e.FromKind = e.Path, "file"
		}
		if e.dstID == 0 {
			e.To = dstName
		}
		if e.Confidence == "alias" && dstName != "" {
			e.Via = dstName
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (e edgeOut) tag() string { return "[" + e.Source + " " + e.Confidence + "]" }

func (e edgeOut) name(callers bool) string {
	n := e.To
	if callers {
		n = e.From
		if e.FromKind == "file" {
			n += " (top level)"
		}
	}
	if e.Via != "" {
		n += " (via " + e.Via + ")"
	}
	return n
}

// graphSymbol resolves the SYMBOL argument like `show`; an alias is followed
// to the function it stands for. It prints the candidates on ambiguity and
// returns ok=false after printing its own error.
func graphSymbol(ctx context.Context, st *store.Store, root, cmd, arg string, errw io.Writer) (symRow, bool) {
	got, err := resolveSymbol(ctx, st, root, arg)
	switch {
	case err != nil:
		fmt.Fprintf(errw, "%s: %v\n", cmd, err)
		return symRow{}, false
	case len(got) == 0:
		fmt.Fprintf(errw, "%s: no symbol matches %q\n", cmd, arg)
		return symRow{}, false
	case len(got) > 1:
		fmt.Fprintf(errw, "%s: %q is ambiguous (%d candidates); pick one by qualified name or path:line\n", cmd, arg, len(got))
		for _, g := range got {
			fmt.Fprintf(errw, "  %s %s  %s:%d\n", g.Kind, g.Qualified, g.Path, g.StartLine)
		}
		return symRow{}, false
	}
	r := got[0]
	if r.Kind == "alias" {
		var dst int64
		_ = st.DB().QueryRowContext(ctx, `SELECT COALESCE(dst_symbol_id,0) FROM edges WHERE kind='alias_of' AND src_symbol_id=?`, r.ID).Scan(&dst)
		if dst != 0 {
			rows, err := st.DB().QueryContext(ctx, symSelect+` WHERE s.id=?`, dst)
			if err == nil {
				if t, _ := scanSyms(rows); len(t) == 1 {
					fmt.Fprintf(errw, "%s: %s is an alias of %s\n", cmd, r.Qualified, t[0].Qualified)
					return t[0], true
				}
			}
		}
	}
	return r, true
}

type symRef struct {
	Qualified string `json:"qualified_name"`
	Kind      string `json:"kind"`
	Path      string `json:"path"`
	Line      int    `json:"line"`
}

func ref(r symRow) symRef { return symRef{r.Qualified, r.Kind, r.Path, r.StartLine} }

func printJSON(out io.Writer, v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Fprintln(out, string(b))
}

func noEdgesNote(ctx context.Context, st *store.Store, errw io.Writer, what string) {
	n := count(ctx, st.DB(), `SELECT count(*) FROM edges WHERE kind='calls'`)
	if n == 0 {
		fmt.Fprintf(errw, "no %s found; the index has no call edges (run `code-index index` without --no-edges)\n", what)
	} else {
		fmt.Fprintf(errw, "no %s found (the index has %d call edge(s))\n", what, n)
	}
}

func cmdCallers(args []string, out, errw io.Writer) int {
	return cmdNeighbors("callers", true, args, out, errw)
}
func cmdCallees(args []string, out, errw io.Writer) int {
	return cmdNeighbors("callees", false, args, out, errw)
}

func cmdNeighbors(name string, callers bool, args []string, out, errw io.Writer) int {
	fs := newFlags(name, errw)
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := parseInterspersed(fs, args)
	if err != nil || len(pos) != 1 {
		fmt.Fprintf(errw, "%s: exactly one SYMBOL is required\n", name)
		return 2
	}
	st, root, err := openExisting()
	if err != nil {
		return fail(errw, "%s: %v", name, err)
	}
	defer st.Close()
	ctx := context.Background()
	var sym symRow
	var edges []edgeOut
	var fileID int64
	if !callers {
		// A file path names the calls made at the top level of that file.
		p := filepath.ToSlash(pos[0])
		_ = st.DB().QueryRowContext(ctx, `SELECT id FROM files WHERE path = ?1 OR path LIKE '%/' || ?1`, p).Scan(&fileID)
		if fileID != 0 {
			if g, _ := resolveSymbol(ctx, st, root, pos[0]); len(g) > 0 {
				fileID = 0 // a symbol of that name wins
			}
		}
	}
	if fileID != 0 {
		sym = symRow{Qualified: pos[0], Kind: "file", Path: pos[0], StartLine: 1}
		edges, err = queryEdges(ctx, st.DB(), `WHERE e.kind='calls' AND e.src_symbol_id IS NULL AND e.file_id=?`, fileID)
	} else {
		var ok bool
		if sym, ok = graphSymbol(ctx, st, root, name, pos[0], errw); !ok {
			return 1
		}
		where := `WHERE e.kind='calls' AND e.src_symbol_id=?`
		if callers {
			where = `WHERE e.kind='calls' AND e.dst_symbol_id=?`
		}
		edges, err = queryEdges(ctx, st.DB(), where, sym.ID)
	}
	if err != nil {
		return fail(errw, "%s: %v", name, err)
	}
	if *asJSON {
		if edges == nil {
			edges = []edgeOut{}
		}
		printJSON(out, struct {
			Symbol symRef    `json:"symbol"`
			Edges  []edgeOut `json:"edges"`
		}{ref(sym), edges})
		return 0
	}
	for _, e := range edges {
		fmt.Fprintf(out, "%s:%d  %s  %s\n", e.Path, e.Line, e.name(callers), e.tag())
	}
	if len(edges) == 0 {
		noEdgesNote(ctx, st, errw, name+" of "+sym.Qualified)
	}
	return 0
}

func cmdImpact(args []string, out, errw io.Writer) int {
	fs := newFlags("impact", errw)
	depth := fs.Int("depth", 3, "how many caller levels to follow")
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := parseInterspersed(fs, args)
	if err != nil || len(pos) != 1 || *depth < 1 {
		fmt.Fprintln(errw, "impact: usage: impact SYMBOL [--depth N] [--json] (N >= 1)")
		return 2
	}
	d := min(*depth, 25)
	st, root, err := openExisting()
	if err != nil {
		return fail(errw, "impact: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	sym, ok := graphSymbol(ctx, st, root, "impact", pos[0], errw)
	if !ok {
		return 1
	}
	// Breadth-first over callers. A symbol is expanded once, so cycles end and
	// every edge is listed once; top-level code (no symbol) has no callers.
	visited := map[int64]bool{sym.ID: true}
	frontier := []int64{sym.ID}
	var edges []edgeOut
	for level := 1; level <= d && len(frontier) > 0; level++ {
		var next []int64
		for _, id := range frontier {
			es, err := queryEdges(ctx, st.DB(), `WHERE e.kind='calls' AND e.dst_symbol_id=?`, id)
			if err != nil {
				return fail(errw, "impact: %v", err)
			}
			for _, e := range es {
				e.Depth = level
				edges = append(edges, e)
				if e.srcID != 0 && !visited[e.srcID] {
					visited[e.srcID] = true
					next = append(next, e.srcID)
				}
			}
		}
		frontier = next
	}
	sort.SliceStable(edges, func(i, j int) bool { return edges[i].Depth < edges[j].Depth })
	if *asJSON {
		if edges == nil {
			edges = []edgeOut{}
		}
		printJSON(out, struct {
			Symbol symRef    `json:"symbol"`
			Depth  int       `json:"depth"`
			Edges  []edgeOut `json:"edges"`
		}{ref(sym), d, edges})
		return 0
	}
	for _, e := range edges {
		fmt.Fprintf(out, "d%d  %s:%d  %s -> %s  %s\n", e.Depth, e.Path, e.Line, e.name(true), e.To, e.tag())
	}
	if len(edges) == 0 {
		noEdgesNote(ctx, st, errw, "callers of "+sym.Qualified)
	}
	return 0
}

func cmdPath(args []string, out, errw io.Writer) int {
	fs := newFlags("path", errw)
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := parseInterspersed(fs, args)
	if err != nil || len(pos) != 2 {
		fmt.Fprintln(errw, "path: usage: path FROM TO [--json]")
		return 2
	}
	st, root, err := openExisting()
	if err != nil {
		return fail(errw, "path: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	from, ok := graphSymbol(ctx, st, root, "path", pos[0], errw)
	if !ok {
		return 1
	}
	to, ok := graphSymbol(ctx, st, root, "path", pos[1], errw)
	if !ok {
		return 1
	}
	adj := map[int64][]int64{}
	rows, err := st.DB().QueryContext(ctx, `SELECT DISTINCT src_symbol_id, dst_symbol_id FROM edges
		WHERE kind='calls' AND src_symbol_id IS NOT NULL AND dst_symbol_id IS NOT NULL ORDER BY 1, 2`)
	if err != nil {
		return fail(errw, "path: %v", err)
	}
	for rows.Next() {
		var a, b int64
		if rows.Scan(&a, &b) == nil {
			adj[a] = append(adj[a], b)
		}
	}
	rows.Close()
	prev := map[int64]int64{from.ID: 0}
	queue := []int64{from.ID}
	for len(queue) > 0 {
		if _, reached := prev[to.ID]; reached {
			break
		}
		cur := queue[0]
		queue = queue[1:]
		for _, n := range adj[cur] {
			if _, seen := prev[n]; !seen {
				prev[n] = cur
				queue = append(queue, n)
			}
		}
	}
	if _, reached := prev[to.ID]; !reached || from.ID == to.ID {
		fmt.Fprintf(errw, "path: no call path from %s to %s\n", from.Qualified, to.Qualified)
		return 1
	}
	var chain []int64
	for n := to.ID; n != from.ID; n = prev[n] {
		chain = append([]int64{n}, chain...)
	}
	var edges []edgeOut
	at := from.ID
	for i, n := range chain {
		es, err := queryEdges(ctx, st.DB(), `WHERE e.kind='calls' AND e.src_symbol_id=? AND e.dst_symbol_id=?`, at, n)
		if err != nil || len(es) == 0 {
			return fail(errw, "path: lost the edge %d -> %d", at, n)
		}
		es[0].Depth = i + 1
		edges = append(edges, es[0])
		at = n
	}
	if *asJSON {
		printJSON(out, struct {
			From  symRef    `json:"from"`
			To    symRef    `json:"to"`
			Edges []edgeOut `json:"edges"`
		}{ref(from), ref(to), edges})
		return 0
	}
	for _, e := range edges {
		fmt.Fprintf(out, "%s:%d  %s -> %s  %s\n", e.Path, e.Line, e.From, e.name(false), e.tag())
	}
	return 0
}
