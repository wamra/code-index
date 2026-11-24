package extract

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/wamra/code-index/internal/lsp"
)

// retryState adapts the empty-answer retry to the server: once a retry has
// produced symbols the server is known to analyse documents asynchronously
// and retries stay on; after three retries that found nothing they stop (the
// files really are empty of symbols), so a server that answers promptly is not
// slowed down. It is the indexer's retryState.
type retryState struct {
	helped, wasted int
	delays         []time.Duration
}

func (r *retryState) allow() bool { return r.helped > 0 || r.wasted < 3 }

// documentSymbols asks for the symbols of an open document and, for an empty
// answer on a non-blank text, asks again after each of the retry delays.
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

// Symbols is phase 1: per file, didOpen, documentSymbol (with the empty-answer
// retry), hover for the hover kinds, didClose; the result is emitted as symbol
// records. A failing documentSymbol (or an unreadable file) is an error record
// in that file's emit; a dead server or a cancelled context is returned.
func (s *lspSession) Symbols(ctx context.Context, files []string, emit Emit) error {
	rs := &retryState{delays: s.delays}
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		recs, err := s.fileSymbols(ctx, f, rs)
		if err != nil {
			if !isPerFile(err) {
				return err
			}
			recs = []Record{{T: TypeError, File: f, Message: err.Error()}}
		}
		if err := emit(f, recs); err != nil {
			return err
		}
	}
	return nil
}

// fileError marks an error that concerns one file only.
type fileError struct{ err error }

func (e fileError) Error() string { return e.err.Error() }
func (e fileError) Unwrap() error { return e.err }

func isPerFile(err error) bool {
	var fe fileError
	return errors.As(err, &fe)
}

func (s *lspSession) fileSymbols(ctx context.Context, file string, rs *retryState) ([]Record, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fileError{err}
	}
	text := normalizeText(data)
	lines := splitSourceLines(text)
	uri := lsp.PathToURI(file)
	if err := s.cl.DidOpen(uri, s.ex.languageID(), 1, text); err != nil {
		return nil, fmt.Errorf("didOpen %s: %w", file, err)
	}
	defer s.cl.DidClose(uri)
	raw, err := documentSymbols(ctx, s.cl, uri, text, rs)
	if err != nil {
		if errors.Is(err, lsp.ErrClosed) || ctx.Err() != nil {
			return nil, err
		}
		return nil, fileError{err}
	}
	normalizeNames(raw, lines)
	syms := flattenSymbols(raw)
	recs := make([]Record, 0, len(syms))
	for i := range syms {
		sym := &syms[i]
		locateName(sym, lines)
		r := symbolRecord(file, sym)
		if hoverKinds[sym.Kind] && s.caps.Hover {
			h, herr := s.cl.Hover(ctx, uri, sym.SelectionRange.Start)
			switch {
			case herr == nil:
				if sig, doc := lsp.ParseHover(h); sig != "" || doc != "" {
					r.Signature, r.Doc = sig, doc
				}
			case errors.Is(herr, lsp.ErrClosed):
				return nil, herr
			case ctx.Err() != nil:
				return nil, ctx.Err()
			}
		}
		recs = append(recs, r)
	}
	return recs, nil
}

// symbolRecord is the symbol record of one flattened symbol: LSP positions
// (zero-based) become the record's one-based ones here, once. The signature
// and doc are filled from hover by the caller; the indexer derives the missing
// ones from detail and the source text (recordSymbols).
func symbolRecord(file string, s *lspSym) Record {
	r := Record{
		T: TypeSymbol, File: file, Name: s.Name, Kind: s.Kind,
		Line: s.Range.Start.Line + 1, Col: s.Range.Start.Character + 1,
		EndLine: s.Range.End.Line + 1, EndCol: s.Range.End.Character + 1,
		SelLine: s.SelectionRange.Start.Line + 1, SelCol: s.SelectionRange.Start.Character + 1,
		Qualified: s.Qualified, Detail: s.Detail,
	}
	if s.Parent >= 0 {
		p := s.Parent
		r.Parent = &p
	}
	return r
}
