package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/wamra/code-index/internal/provider"
	"github.com/wamra/code-index/internal/search"
)

func cmdSearch(args []string, out, errw io.Writer) int {
	fs := newFlags("search", errw)
	mode := fs.String("mode", "auto", "auto|vector|fts|hybrid")
	k := fs.Int("k", 10, "max hits")
	kind := fs.String("kind", "", "symbol kind (function, class, method, ...) or file")
	lang := fs.String("lang", "", "language name from the config")
	pathSub := fs.String("path", "", "only files whose path contains this text")
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) == 0 {
		fmt.Fprintln(errw, "search: QUERY is required")
		return 2
	}
	st, root, err := openExisting()
	if err != nil {
		return fail(errw, "search: %v", err)
	}
	defer st.Close()
	cfg, _, err := loadConfig(root)
	if err != nil {
		return fail(errw, "search: %v", err)
	}
	emb, err := newEmbedder(cfg, st)
	if err != nil {
		return fail(errw, "search: %v", err)
	}
	hits, route, err := search.Run(context.Background(), st, strings.Join(pos, " "), search.Opts{
		Mode: *mode, K: *k, Kind: *kind, Lang: *lang, Path: *pathSub,
		Embedder: emb, Root: root,
		Warn: func(m string) { fmt.Fprintf(errw, "search: warning: %s\n", m) },
	})
	if err != nil {
		switch {
		case errors.Is(err, provider.ErrAuth):
			return fail(errw, "search: authentication failed for %s (try --mode fts)", cfg.Embed.Provider)
		case errors.Is(err, provider.ErrDown):
			return fail(errw, "search: %s unreachable at %s (try --mode fts)", cfg.Embed.Provider, cfg.Embed.BaseURL)
		case errors.Is(err, search.ErrNoVectors):
			return fail(errw, "search: %v (try --mode fts)", err)
		}
		return fail(errw, "search: %v", err)
	}
	if *asJSON {
		if hits == nil {
			hits = []search.Hit{}
		}
		b, err := json.MarshalIndent(map[string]any{"hits": hits, "route": route}, "", "  ")
		if err != nil {
			return fail(errw, "search: %v", err)
		}
		fmt.Fprintln(out, string(b))
		return 0
	}
	for _, h := range hits {
		stale := ""
		if h.Stale != "" {
			stale = "  [stale: " + h.Stale + "]"
		}
		fmt.Fprintf(out, "[%.3f] %s %s  %s:%d%s\n", h.Score, h.Kind, h.Qualified, h.Path, h.Line, stale)
		if h.Signature != "" {
			fmt.Fprintf(out, "        %s\n", oneLine(h.Signature, 160))
		}
		if h.Summary != "" {
			fmt.Fprintf(out, "        %s\n", oneLine(h.Summary, 160))
		}
	}
	fmt.Fprintf(out, "route: %s\n", route)
	return 0
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}
