package indexer

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"unicode/utf16"

	"github.com/wamra/code-index/internal/extract"
	"github.com/wamra/code-index/internal/lsp"
	"github.com/wamra/code-index/internal/store"
)

// recordSymbols turns the symbol and func records of one file into the symbols
// writeFile stores, as flatten does for a documentSymbol response. lines is the
// file's text split by splitLines.
//
// Positions are 1-based in the records and zero-based in flatSym. A record
// without selLine/selCol has its name located in the first line of its range
// (locateName). Missing fields are filled the way indexFile fills them for
// a language server that answers without hover: Signature from the record,
// else its detail, else the declaration line; Doc from the record, else the
// comment lines above the definition, else a PowerShell help block; BodySHA
// from the lines the definition spans. The Doc and declaration fallbacks apply
// to the kinds that get hover in indexFile (hoverKinds). The parent of a record
// is the record whose zero-based ordinal (among the file's symbol and func
// records, counting every one) the "parent" key names; a missing, forward or
// unusable parent makes the symbol top level. A record without a name is skipped.
func recordSymbols(recs []extract.Record, lines []string) []flatSym {
	var out []flatSym
	flatOf := map[int]int{} // record ordinal -> index in out
	ord := -1
	for _, r := range recs {
		if !r.IsSymbol() {
			continue
		}
		ord++
		if r.Name == "" {
			continue
		}
		kind := strings.ToLower(r.Kind)
		if r.T == extract.TypeFunc && kind == "" {
			kind = "function"
		}
		if kind == "" {
			kind = "unknown"
		}
		start := lsp.Position{Line: max(r.Line-1, 0), Character: max(r.Col-1, 0)}
		end := lsp.Position{Line: max(r.EndLine-1, start.Line), Character: max(r.EndCol-1, 0)}
		if r.EndLine == 0 || (end.Line == start.Line && end.Character < start.Character) {
			end = start
		}
		s := lsp.Symbol{Name: r.Name, Detail: r.Detail, Kind: kind, Range: lsp.Range{Start: start, End: end}}
		s.SelectionRange = s.Range
		if r.SelLine > 0 {
			sel := lsp.Position{Line: r.SelLine - 1, Character: max(r.SelCol-1, 0)}
			s.SelectionRange = lsp.Range{Start: sel, End: lsp.Position{Line: sel.Line, Character: sel.Character + len(utf16.Encode([]rune(r.Name)))}}
		}
		parent := -1
		if r.Parent != nil {
			if p, ok := flatOf[*r.Parent]; ok && *r.Parent < ord {
				parent = p
			}
		}
		q := r.Qualified
		if q == "" {
			q = r.Name
			if parent >= 0 {
				q = out[parent].Qualified + "." + r.Name
			}
		}
		f := flatSym{Symbol: s, Parent: parent, Qualified: q, Signature: r.Signature, Doc: r.Doc}
		locateName(&f, lines)
		f.BodySHA = bodySHA(lines, f.Range.Start.Line, f.Range.End.Line)
		if f.Signature == "" {
			f.Signature = f.Detail
		}
		if hoverKinds[kind] {
			if f.Doc == "" {
				f.Doc = leadingComment(lines, f.Range.Start.Line)
			}
			if f.Doc == "" {
				f.Doc = helpComment(lines, f.Range.Start.Line, f.Range.End.Line)
			}
			if f.Signature == "" {
				f.Signature = declarationLine(lines, f.Range.Start.Line)
			}
		}
		flatOf[ord] = len(out)
		out = append(out, f)
	}
	return out
}

// indexFromRecords replaces the rows of one file with the symbols of its
// symbol and func records, like indexFile does with a language server's answer.
// The symbols are stored with symbols.source 'extract': 'ast' symbols are
// deleted with every edge rebuild (deleteLangEdges), and these belong to the
// file's own indexing, which unchanged files keep.
func indexFromRecords(ctx context.Context, st *store.Store, c candidate, data []byte, sha string, oldID int64, hasVec bool, recs []extract.Record) (fileStats, error) {
	text := strings.ToValidUTF8(string(data), "\ufffd")
	text = strings.TrimPrefix(text, "\ufeff") // the text the positions refer to, as for a language server
	syms := recordSymbols(recs, splitLines(text))
	return writeFileSource(ctx, st, c, sha, oldID, hasVec, syms, symSourceExtract)
}

// knownSymbols returns the symbol records of every indexed ('ok') file of lang,
// the "known" argument of extract.Session.Edges. Each record has t "symbol",
// the absolute file, name, kind, qualified, the whole definition (line, col,
// endLine, endCol), the name position (selLine, selCol: the stored selection,
// the definition start when none), signature and doc when stored, and
// parent as the ordinal among the same file's records in this order (files by
// path; within a file by start position, then id; alias symbols, source 'ast',
// are left out because they are rebuilt with the edges). A parent always
// precedes its children.
func knownSymbols(ctx context.Context, db *sql.DB, root, lang string) ([]extract.Record, error) {
	rows, err := db.QueryContext(ctx, `SELECT s.id, s.parent_id, f.path, s.name, s.kind, s.qualified_name,
		s.start_line, s.start_col, s.end_line, s.end_col, COALESCE(s.sel_line, s.start_line), COALESCE(s.sel_col, s.start_col),
		COALESCE(s.signature,''), COALESCE(s.doc,'')
		FROM symbols s JOIN files f ON f.id = s.file_id
		WHERE f.lang=? AND f.status='ok' AND s.source <> 'ast' ORDER BY f.path, s.start_line, s.start_col, s.id`, lang)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []extract.Record
	ordOf := map[int64]int{}
	curFile, base := "", 0
	for rows.Next() {
		var id int64
		var parent sql.NullInt64
		var rel string
		r := extract.Record{T: extract.TypeSymbol}
		var sl, sc, el, ec, sell, selc int
		if err := rows.Scan(&id, &parent, &rel, &r.Name, &r.Kind, &r.Qualified, &sl, &sc, &el, &ec, &sell, &selc, &r.Signature, &r.Doc); err != nil {
			return nil, err
		}
		if rel != curFile {
			curFile, base = rel, len(out)
		}
		r.File = filepath.Join(root, filepath.FromSlash(rel))
		r.Line, r.Col, r.EndLine, r.EndCol = sl+1, sc+1, el+1, ec+1
		r.SelLine, r.SelCol = sell+1, selc+1
		if parent.Valid {
			if p, ok := ordOf[parent.Int64]; ok {
				r.Parent = &p
			}
		}
		ordOf[id] = len(out) - base
		out = append(out, r)
	}
	return out, rows.Err()
}
