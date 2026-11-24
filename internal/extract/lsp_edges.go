package extract

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"unicode/utf16"

	"github.com/wamra/code-index/internal/lsp"
)

// Resolution and reason of the call records an lsp session writes.
const (
	reasonCallHierarchy = "call-hierarchy"
	reasonReferences    = "references"
)

// target is a callable symbol of a file, from the known records.
type target struct {
	name      string
	file      string
	line, col int // zero-based name position
}

// callSiteKey identifies a call record so repeats (several hierarchy items
// for one symbol, a server repeating a location) are written once.
type callSiteKey struct {
	file, to      string
	line, col     int
	toLine, toCol int
	name          string
	reason        string
}

// edgeRun is the state of one Edges call.
type edgeRun struct {
	s          *lspSession
	files      []string          // requested files, as given
	byURI      map[string]string // URI -> requested file (also caches SameFile matches)
	records    map[string][]Record
	seen       map[callSiteKey]bool
	reqs       int
	failed     int
	first      error
	firstFile  string
	useHier    bool
	reasonText string
}

// Edges is phase 2. For every callable symbol of known it asks the server who
// calls it: callHierarchy (prepareCallHierarchy at the name, then
// incomingCalls) when advertised, else references. Each call site becomes a
// call record in the file of the site, once per requested file; call sites in
// files that were not requested are dropped. A failing request is counted and
// skipped (one error record on the file of the first failure and an Env.Logf
// line report it); a dead server or a cancelled context is returned.
func (s *lspSession) Edges(ctx context.Context, files []string, known []Record, emit Emit) error {
	run := &edgeRun{
		s: s, files: files, byURI: map[string]string{},
		records: map[string][]Record{}, seen: map[callSiteKey]bool{},
		useHier: s.caps.CallHierarchy,
	}
	run.reasonText = reasonReferences
	if run.useHier {
		run.reasonText = reasonCallHierarchy
	}
	for _, f := range files {
		run.byURI[lsp.PathToURI(f)] = f
	}
	if err := run.collect(ctx, known); err != nil {
		return err
	}
	for _, recs := range run.records {
		sortRecords(recs)
	}
	s.logf("  %s: %d %s request(s)", s.ex.lang, run.reqs, run.reasonText)
	if run.failed > 0 {
		s.logf("  %s: %d of %d %s request(s) failed (first: %v)", s.ex.lang, run.failed, run.reqs, run.reasonText, run.first)
		if f, ok := run.requested(run.firstFile); ok {
			run.records[f] = append(run.records[f], Record{
				T: TypeError, File: f,
				Message: fmt.Sprintf("%d of %d %s request(s) failed (first: %v)", run.failed, run.reqs, run.reasonText, run.first),
			})
		}
	}
	for _, f := range files {
		recs := run.records[f]
		if recs == nil {
			recs = []Record{}
		}
		if err := emit(f, recs); err != nil {
			return err
		}
	}
	return nil
}

// targets groups the callable symbols of known by file, in known's order.
func targets(known []Record) (order []string, byFile map[string][]target) {
	byFile = map[string][]target{}
	for _, r := range known {
		if !r.IsSymbol() || r.Name == "" || r.File == "" {
			continue
		}
		kind := r.Kind
		if r.T == TypeFunc && kind == "" {
			kind = "function"
		}
		if !callableKinds[kind] {
			continue
		}
		line, col := r.SelLine, r.SelCol
		if line == 0 {
			line, col = r.Line, r.Col
		}
		if _, ok := byFile[r.File]; !ok {
			order = append(order, r.File)
		}
		byFile[r.File] = append(byFile[r.File], target{name: r.Name, file: r.File, line: line - 1, col: max(col-1, 0)})
	}
	return order, byFile
}

func (e *edgeRun) collect(ctx context.Context, known []Record) error {
	order, byFile := targets(known)
	for _, file := range order {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		text := normalizeText(data)
		uri := lsp.PathToURI(file)
		if err := e.s.cl.DidOpen(uri, e.s.ex.languageID(), 1, text); err != nil {
			return fmt.Errorf("didOpen %s: %w", file, err)
		}
		for _, t := range byFile[file] {
			if err := ctx.Err(); err != nil {
				e.s.cl.DidClose(uri)
				return err
			}
			e.reqs++
			var err error
			if e.useHier {
				err = e.incoming(ctx, uri, t)
			} else {
				err = e.references(ctx, uri, t)
			}
			if err != nil {
				if errors.Is(err, lsp.ErrClosed) || ctx.Err() != nil {
					e.s.cl.DidClose(uri)
					if ctx.Err() != nil {
						return ctx.Err()
					}
					return err
				}
				e.failed++
				if e.first == nil {
					e.first, e.firstFile = err, file
				}
			}
		}
		e.s.cl.DidClose(uri)
	}
	return nil
}

// incoming asks for the callers of t through call hierarchy; each fromRange is
// an exact call site.
func (e *edgeRun) incoming(ctx context.Context, uri string, t target) error {
	items, err := e.s.cl.PrepareCallHierarchy(ctx, uri, lsp.Position{Line: t.line, Character: t.col})
	if err != nil {
		return err
	}
	for _, it := range items {
		calls, err := e.s.cl.IncomingCalls(ctx, it)
		if err != nil {
			return err
		}
		for _, c := range calls {
			for _, r := range c.FromRanges {
				e.add(c.From.URI, r.Start, t, "exact", reasonCallHierarchy)
			}
		}
	}
	return nil
}

// references asks for the references of t; a reference is not necessarily a
// call, so each one is a candidate. The declaration itself is skipped.
func (e *edgeRun) references(ctx context.Context, uri string, t target) error {
	locs, err := e.s.cl.References(ctx, uri, lsp.Position{Line: t.line, Character: t.col})
	if err != nil {
		return err
	}
	selEnd := t.col + len(utf16.Encode([]rune(t.name)))
	for _, l := range locs {
		p := l.Range.Start
		if lsp.SameFile(l.URI, uri) && p.Line == t.line && p.Character >= t.col && p.Character < selEnd {
			continue // the declaration itself
		}
		e.add(l.URI, p, t, "candidate", reasonReferences)
	}
	return nil
}

// requested maps a path to the requested file it names.
func (e *edgeRun) requested(path string) (string, bool) {
	if path == "" {
		return "", false
	}
	return e.requestedURI(lsp.PathToURI(path))
}

// requestedURI maps a file URI to the requested file it names, tolerating the
// servers' spellings of the same file (SameFile).
func (e *edgeRun) requestedURI(uri string) (string, bool) {
	if f, ok := e.byURI[uri]; ok {
		return f, f != ""
	}
	for u, f := range e.byURI {
		if f != "" && lsp.SameFile(u, uri) {
			e.byURI[uri] = f
			return f, true
		}
	}
	e.byURI[uri] = ""
	return "", false
}

// add records one call site (position p, zero-based, in the file of uri) of the
// callable t. Sites outside the requested files are dropped.
func (e *edgeRun) add(uri string, p lsp.Position, t target, resolution, reason string) {
	file, ok := e.requestedURI(uri)
	if !ok {
		return
	}
	k := callSiteKey{file: file, to: t.file, line: p.Line, col: p.Character, toLine: t.line, toCol: t.col, name: t.name, reason: reason}
	if e.seen[k] {
		return
	}
	e.seen[k] = true
	e.records[file] = append(e.records[file], Record{
		T: TypeCall, File: file, Line: p.Line + 1, Col: p.Character + 1,
		Name: t.name, To: t.file, ToLine: t.line + 1, ToCol: t.col + 1,
		Resolution: resolution, Reason: reason,
	})
}

// sortRecords orders the records of a file by position so the output does not
// depend on the order the server answered in.
func sortRecords(recs []Record) {
	sort.SliceStable(recs, func(i, j int) bool {
		a, b := recs[i], recs[j]
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Col != b.Col {
			return a.Col < b.Col
		}
		return a.To+a.Name < b.To+b.Name
	})
}
