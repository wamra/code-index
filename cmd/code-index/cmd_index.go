package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"

	"github.com/wamra/code-index/internal/config"
	"github.com/wamra/code-index/internal/embed"
	"github.com/wamra/code-index/internal/indexer"
	"github.com/wamra/code-index/internal/provider"
	"github.com/wamra/code-index/internal/store"
	"github.com/wamra/code-index/internal/workspace"
)

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func cmdIndex(args []string, out, errw io.Writer) int {
	fs := newFlags("index", errw)
	doEmbed := fs.Bool("embed", false, "embed pending chunks afterwards")
	force := fs.Bool("force", false, "re-index files even when unchanged")
	quiet := fs.Bool("quiet", false, "print only the summary")
	pos, err := parseInterspersed(fs, args)
	if err != nil || len(pos) > 1 {
		fmt.Fprintln(errw, "index: usage: index [PATH] [--embed] [--force] [--quiet]")
		return 2
	}
	target := "."
	if len(pos) == 1 {
		target = pos[0]
	}
	if info, err := os.Stat(target); err != nil || !info.IsDir() {
		return fail(errw, "index: %s is not a directory", target)
	}
	root := workspace.Root(target)
	st, err := store.Open(workspace.DBPath(root, os.Getenv))
	if err != nil {
		return fail(errw, "index: open database: %v", err)
	}
	defer st.Close()
	if os.Getenv("CODE_INDEX_DB") == "" {
		if p, err := workspace.EnsureExcluded(root); err != nil {
			fmt.Fprintf(errw, "index: could not exclude %s/ from git: %v\n", workspace.DirName, err)
		} else if p != "" {
			fmt.Fprintf(out, "added %s/ to %s\n", workspace.DirName, p)
		}
	}
	return runIndex(st, root, *force, *doEmbed, *quiet, out, errw)
}

func cmdSweep(args []string, out, errw io.Writer) int {
	fs := newFlags("sweep", errw)
	doEmbed := fs.Bool("embed", false, "embed pending chunks afterwards")
	force := fs.Bool("force", false, "re-index files even when unchanged")
	quiet := fs.Bool("quiet", false, "print only the summary")
	if _, err := parseInterspersed(fs, args); err != nil {
		return 2
	}
	st, root, err := openExisting()
	if err != nil {
		return fail(errw, "sweep: %v", err)
	}
	defer st.Close()
	return runIndex(st, root, *force, *doEmbed, *quiet, out, errw)
}

func runIndex(st *store.Store, root string, force, doEmbed, quiet bool, out, errw io.Writer) int {
	cfg, _, err := loadConfig(root)
	if err != nil {
		return fail(errw, "index: %v", err)
	}
	if len(cfg.Languages) == 0 {
		return fail(errw, "index: no languages configured in %s; run `code-index config init`", configPath())
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var logw io.Writer = errw
	if quiet {
		logw = nil
	}
	res, err := indexer.Run(ctx, st, cfg, indexer.Options{Root: root, Force: force, Log: logw})
	if err != nil {
		if errors.Is(err, indexer.ErrLocked) {
			return fail(errw, "index: %v", err)
		}
		return fail(errw, "index: %v", err)
	}
	fmt.Fprintf(out, "%s: %d file(s) of configured languages; indexed %d (%d symbols, %d chunks), unchanged %d, failed %d, deleted %d",
		root, res.Scanned, res.Indexed, res.Symbols, res.Chunks, res.Unchanged, res.Failed, res.Deleted)
	if res.TooLarge > 0 {
		fmt.Fprintf(out, ", skipped too large %d", res.TooLarge)
	}
	if res.Binary > 0 {
		fmt.Fprintf(out, ", skipped binary %d", res.Binary)
	}
	fmt.Fprintf(out, " (%s)\n", res.Elapsed.Round(1e6))
	for _, l := range sortedKeys(res.LangErrors) {
		fmt.Fprintf(errw, "index: language %s unusable: %s\n", l, res.LangErrors[l])
	}
	for _, fe := range res.FileErrors {
		fmt.Fprintf(errw, "index: failed %s\n", fe)
	}
	code := 0
	if res.Failed > 0 || len(res.LangErrors) > 0 {
		code = 1
	}
	if doEmbed {
		if c := runEmbed(ctx, st, cfg, out, errw); c != 0 {
			code = c
		}
	}
	return code
}

func runEmbed(ctx context.Context, st *store.Store, cfg config.Config, out, errw io.Writer) int {
	emb, err := newEmbedder(cfg, st)
	if err != nil {
		return fail(errw, "embed: %v", err)
	}
	if emb == nil {
		return fail(errw, "embed: no embedder configured (set embed.enabled or embed.provider/model in %s); full-text search works without it", configPath())
	}
	cap, _ := cfg.Embed.DailyCap()
	n, err := embed.Drain(ctx, st, emb, embed.Opts{MaxPerDay: cap})
	fmt.Fprintf(out, "embedded %d chunk(s) with %s\n", n, emb.Fingerprint())
	switch {
	case err == nil:
		return 0
	case errors.Is(err, embed.ErrLocked):
		return fail(errw, "embed: another embedding run holds the lock")
	case errors.Is(err, provider.ErrAuth):
		return fail(errw, "embed: authentication failed for %s (api_key_env %q: %s)", cfg.Embed.Provider, cfg.Embed.APIKeyEnv, provider.KeyState(cfg.Embed.APIKeyEnv))
	case errors.Is(err, provider.ErrDown):
		return fail(errw, "embed: %s unreachable at %s; chunks stay pending", cfg.Embed.Provider, cfg.Embed.BaseURL)
	}
	return fail(errw, "embed: %v", err)
}
