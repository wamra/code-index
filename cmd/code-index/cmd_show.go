package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/wamra/code-index/internal/search"
	"github.com/wamra/code-index/internal/store"
)

type symRow struct {
	ID          int64    `json:"id"`
	Name        string   `json:"name"`
	Qualified   string   `json:"qualified_name"`
	Kind        string   `json:"kind"`
	Path        string   `json:"path"`
	Lang        string   `json:"lang"`
	StartLine   int      `json:"start_line"` // 1-based
	EndLine     int      `json:"end_line"`
	Signature   string   `json:"signature,omitempty"`
	Doc         string   `json:"doc,omitempty"`
	Parent      string   `json:"parent,omitempty"`
	Children    []string `json:"children,omitempty"`
	Stale       string   `json:"stale,omitempty"`
	mtime, size int64
	sha         string
}

const symSelect = `SELECT s.id, s.name, s.qualified_name, s.kind, f.path, f.lang, s.start_line+1, s.end_line+1,
	COALESCE(s.signature,''), COALESCE(s.doc,''), COALESCE(p.qualified_name,''), COALESCE(f.mtime,0), COALESCE(f.size,0), f.sha256
	FROM symbols s JOIN files f ON f.id = s.file_id LEFT JOIN symbols p ON p.id = s.parent_id`

func scanSyms(rows *sql.Rows) ([]symRow, error) {
	defer rows.Close()
	var out []symRow
	for rows.Next() {
		var r symRow
		if err := rows.Scan(&r.ID, &r.Name, &r.Qualified, &r.Kind, &r.Path, &r.Lang, &r.StartLine, &r.EndLine, &r.Signature, &r.Doc, &r.Parent, &r.mtime, &r.size, &r.sha); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

var pathLineRe = regexp.MustCompile(`^(.+):(\d+)$`)

// resolveSymbol finds the symbols a SYMBOL argument names: path:line (the
// innermost symbol covering that line), else a qualified name, else a name,
// each tier case-sensitive first, then case-insensitive.
func resolveSymbol(ctx context.Context, st *store.Store, root, arg string) ([]symRow, error) {
	db := st.DB()
	if m := pathLineRe.FindStringSubmatch(arg); m != nil {
		line, _ := strconv.Atoi(m[2])
		p := filepath.ToSlash(m[1])
		if filepath.IsAbs(m[1]) {
			if rel, err := filepath.Rel(root, m[1]); err == nil {
				p = filepath.ToSlash(rel)
			}
		}
		p = strings.TrimPrefix(p, "./")
		rows, err := db.QueryContext(ctx, symSelect+` WHERE (f.path = ?1 OR f.path LIKE '%/' || ?1) AND s.start_line+1 <= ?2 AND s.end_line+1 >= ?2
			ORDER BY (s.end_line - s.start_line), s.id`, p, line)
		if err != nil {
			return nil, err
		}
		got, err := scanSyms(rows)
		if err != nil || len(got) > 0 {
			if len(got) > 1 { // keep only the innermost span
				best := got[0].EndLine - got[0].StartLine
				n := 0
				for _, g := range got {
					if g.EndLine-g.StartLine == best {
						n++
					}
				}
				got = got[:n]
			}
			return got, err
		}
	}
	for _, q := range []string{
		` WHERE s.qualified_name = ?`, ` WHERE s.name = ?`,
		` WHERE lower(s.qualified_name) = lower(?)`, ` WHERE lower(s.name) = lower(?)`,
	} {
		rows, err := db.QueryContext(ctx, symSelect+q+` ORDER BY f.path, s.start_line, s.id`, arg)
		if err != nil {
			return nil, err
		}
		got, err := scanSyms(rows)
		if err != nil || len(got) > 0 {
			return got, err
		}
	}
	return nil, nil
}

func cmdShow(args []string, out, errw io.Writer) int {
	fs := newFlags("show", errw)
	source := fs.Bool("source", false, "also print the symbol's source lines from disk")
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := parseInterspersed(fs, args)
	if err != nil || len(pos) != 1 {
		fmt.Fprintln(errw, "show: exactly one SYMBOL is required")
		return 2
	}
	st, root, err := openExisting()
	if err != nil {
		return fail(errw, "show: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	got, err := resolveSymbol(ctx, st, root, pos[0])
	if err != nil {
		return fail(errw, "show: %v", err)
	}
	switch {
	case len(got) == 0:
		return fail(errw, "show: no symbol matches %q", pos[0])
	case len(got) > 1:
		fmt.Fprintf(errw, "show: %q is ambiguous (%d candidates); pick one by qualified name or path:line\n", pos[0], len(got))
		for _, g := range got {
			fmt.Fprintf(errw, "  %s %s  %s:%d\n", g.Kind, g.Qualified, g.Path, g.StartLine)
		}
		return 1
	}
	r := got[0]
	rows, err := st.DB().QueryContext(ctx, `SELECT kind || ' ' || qualified_name FROM symbols WHERE parent_id = ? ORDER BY start_line, id`, r.ID)
	if err == nil {
		for rows.Next() {
			var c string
			if rows.Scan(&c) == nil {
				r.Children = append(r.Children, c)
			}
		}
		rows.Close()
	}
	r.Stale = search.Staleness(root, r.Path, r.mtime, r.size, r.sha)
	var src string
	if *source {
		if data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(r.Path))); err == nil {
			lines := strings.Split(string(data), "\n")
			lo, hi := r.StartLine-1, min(r.EndLine, len(lines))
			if lo >= 0 && lo < hi {
				src = strings.Join(lines[lo:hi], "\n")
			}
		}
	}
	if *asJSON {
		b, _ := json.MarshalIndent(struct {
			symRow
			Source string `json:"source,omitempty"`
		}{r, src}, "", "  ")
		fmt.Fprintln(out, string(b))
		return 0
	}
	fmt.Fprintf(out, "%s %s\n", r.Kind, r.Qualified)
	fmt.Fprintf(out, "  file:      %s:%d-%d (%s)\n", r.Path, r.StartLine, r.EndLine, r.Lang)
	if r.Signature != "" {
		fmt.Fprintf(out, "  signature: %s\n", oneLine(r.Signature, 300))
	}
	if r.Doc != "" {
		fmt.Fprintf(out, "  doc:       %s\n", oneLine(r.Doc, 600))
	}
	if r.Parent != "" {
		fmt.Fprintf(out, "  parent:    %s\n", r.Parent)
	}
	for _, c := range r.Children {
		fmt.Fprintf(out, "  child:     %s\n", c)
	}
	if r.Stale != "" {
		fmt.Fprintf(out, "  stale:     file %s since it was indexed\n", r.Stale)
	}
	if src != "" {
		fmt.Fprintf(out, "---\n%s\n", src)
	}
	return 0
}
