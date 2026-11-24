package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/wamra/code-index/internal/config"
	"github.com/wamra/code-index/internal/embed"
	"github.com/wamra/code-index/internal/extract"
	"github.com/wamra/code-index/internal/provider"
	"github.com/wamra/code-index/internal/search"
	"github.com/wamra/code-index/internal/store"
	"github.com/wamra/code-index/internal/workspace"
)

func count(ctx context.Context, db *sql.DB, q string, args ...any) int {
	var n int
	_ = db.QueryRowContext(ctx, q, args...).Scan(&n)
	return n
}

// breakdown renders "a 3, b 2" from a two-column (label, n) query.
func breakdown(ctx context.Context, db *sql.DB, q string) string {
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return err.Error()
	}
	defer rows.Close()
	var parts []string
	for rows.Next() {
		var l string
		var n int
		if rows.Scan(&l, &n) == nil {
			parts = append(parts, fmt.Sprintf("%s %d", l, n))
		}
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ", ")
}

func cmdStats(args []string, out, errw io.Writer) int {
	fs := newFlags("stats", errw)
	if _, err := parseInterspersed(fs, args); err != nil {
		return 2
	}
	st, root, err := openExisting()
	if err != nil {
		return fail(errw, "stats: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	db := st.DB()
	size := int64(0)
	if fi, err := os.Stat(st.Path()); err == nil {
		size = fi.Size()
	}
	fmt.Fprintf(out, "workspace:   %s\n", root)
	fmt.Fprintf(out, "database:    %s (%.1f KiB)\n", st.Path(), float64(size)/1024)
	last, _ := st.Meta(ctx, "last_index_at")
	if last == "" {
		last = "never"
	}
	fmt.Fprintf(out, "last index:  %s\n", last)
	fmt.Fprintf(out, "files:       %d (%s)\n", count(ctx, db, `SELECT count(*) FROM files`),
		breakdown(ctx, db, `SELECT status, count(*) FROM files GROUP BY status ORDER BY status`))
	fmt.Fprintf(out, "  by lang:   %s\n", breakdown(ctx, db, `SELECT lang, count(*) FROM files GROUP BY lang ORDER BY lang`))
	fmt.Fprintf(out, "symbols:     %d\n", count(ctx, db, `SELECT count(*) FROM symbols`))
	fmt.Fprintf(out, "  by kind:   %s\n", breakdown(ctx, db, `SELECT kind, count(*) FROM symbols GROUP BY kind ORDER BY count(*) DESC, kind`))
	total := count(ctx, db, `SELECT count(*) FROM chunks`)
	done := count(ctx, db, `SELECT count(*) FROM chunks WHERE embedded_at IS NOT NULL AND embedded_at <> 'failed'`)
	failed := count(ctx, db, `SELECT count(*) FROM chunks WHERE embedded_at = 'failed'`)
	fmt.Fprintf(out, "chunks:      %d (embedded %d, pending %d, failed %d)\n", total, done, total-done-failed, failed)
	fmt.Fprintf(out, "edges:       %d\n", count(ctx, db, `SELECT count(*) FROM edges`))
	fmt.Fprintf(out, "  by kind:   %s\n", breakdown(ctx, db, `SELECT kind, count(*) FROM edges GROUP BY kind ORDER BY count(*) DESC, kind`))
	fmt.Fprintf(out, "  by source: %s\n", breakdown(ctx, db, `SELECT source, count(*) FROM edges GROUP BY source ORDER BY count(*) DESC, source`))
	fmt.Fprintf(out, "  by conf:   %s\n", breakdown(ctx, db, `SELECT confidence, count(*) FROM edges GROUP BY confidence ORDER BY count(*) DESC, confidence`))
	fmt.Fprintf(out, "summaries:   %d (not populated before milestone 3)\n", count(ctx, db, `SELECT count(*) FROM summaries`))
	fp, _ := st.Meta(ctx, "embed_fingerprint")
	dims, _ := st.VecDims(ctx)
	if fp == "" {
		fmt.Fprintln(out, "embedder:    none recorded")
	} else {
		fmt.Fprintf(out, "embedder:    %s (%d dims)\n", fp, dims)
	}
	return 0
}

type check struct{ level, name, detail string }

func cmdDoctor(args []string, out, errw io.Writer) int {
	fs := newFlags("doctor", errw)
	noIndex := fs.Bool("no-index", false, "check the configuration, language servers and embedder only; skip the index of this directory")
	if _, err := parseInterspersed(fs, args); err != nil {
		return 2
	}
	var checks []check
	add := func(level, name, format string, a ...any) {
		checks = append(checks, check{level, name, fmt.Sprintf(format, a...)})
	}
	ctx := context.Background()

	cfg, csrc, cerr := loadConfig(cwdRoot())
	if cerr != nil {
		add("FAIL", "config", "%v", cerr)
	} else {
		files := map[string]bool{}
		for _, f := range csrc {
			if f != "default" {
				files[f] = true
			}
		}
		add("ok", "config", "%d language(s); files read: %s", len(cfg.Languages), joinKeys(files))
	}

	if *noIndex {
		if cerr == nil {
			for _, name := range cfg.LanguageNames() {
				level, detail := languageCheck(cfg, cwdRoot(), name)
				add(level, "language "+name, "%s", detail)
			}
			if !cfg.EmbedEnabled() {
				add("ok", "embedder", "not configured: search uses names and full text only")
			} else if emb, err := newEmbedder(cfg, nil); err != nil {
				add("FAIL", "embedder", "%v", err)
			} else {
				hctx, cancel := context.WithTimeout(ctx, 15*time.Second)
				herr := emb.Health(hctx)
				cancel()
				if herr != nil {
					add("WARN", "embedder", "%s %s: %v", cfg.Embed.Provider, cfg.Embed.Model, herr)
				} else {
					add("ok", "embedder", "%s %s reachable (api key: %s)", cfg.Embed.Provider, cfg.Embed.Model, provider.KeyState(cfg.Embed.APIKeyEnv))
				}
			}
		}
		return printChecks(checks, out)
	}

	st, root, err := openExisting()
	if err != nil {
		add("FAIL", "index", "%v", err)
		return printChecks(checks, out)
	}
	defer st.Close()
	db := st.DB()
	ver, _ := st.Meta(ctx, "schema_version")
	add("ok", "index", "%s (schema %s)", st.Path(), ver)
	if os.Getenv("CODE_INDEX_DB") != "" {
		add("ok", "git-ignore", "CODE_INDEX_DB is set: the database is not under %s/", workspace.DirName)
	} else if ig, known := workspace.GitIgnored(root); !known {
		add("ok", "git-ignore", "%s/ not checked (%s is not a git repo, or git is unavailable)", workspace.DirName, root)
	} else if ig {
		add("ok", "git-ignore", "%s/ is ignored by git", workspace.DirName)
	} else {
		add("WARN", "git-ignore", "%s/ is NOT ignored by git; `code-index index` adds it to the repo's info/exclude", workspace.DirName)
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		add("FAIL", "workspace", "root %s is not accessible", root)
	} else {
		add("ok", "workspace", "%s", root)
	}

	for _, t := range []string{"chunk_fts", "name_fts"} {
		if err := store.FTSCommand(ctx, db, t, "integrity-check"); err != nil {
			add("FAIL", t, "integrity check: %v", err)
		} else {
			add("ok", t, "integrity check passed")
		}
	}
	syms, nameRows := count(ctx, db, `SELECT count(*) FROM symbols`), count(ctx, db, `SELECT count(*) FROM name_fts`)
	chunks, ftsRows := count(ctx, db, `SELECT count(*) FROM chunks`), count(ctx, db, `SELECT count(*) FROM chunk_fts`)
	if syms != nameRows || chunks != ftsRows {
		add("FAIL", "fts rows", "symbols %d vs name_fts %d, chunks %d vs chunk_fts %d (re-run `code-index index --force`)", syms, nameRows, chunks, ftsRows)
	} else {
		add("ok", "fts rows", "symbols %d = name_fts %d, chunks %d = chunk_fts %d", syms, nameRows, chunks, ftsRows)
	}

	if cerr == nil {
		used := map[string]int{}
		rows, _ := db.QueryContext(ctx, `SELECT lang, count(*) FROM files GROUP BY lang`)
		if rows != nil {
			for rows.Next() {
				var l string
				var n int
				if rows.Scan(&l, &n) == nil {
					used[l] = n
				}
			}
			rows.Close()
		}
		for _, name := range cfg.LanguageNames() {
			level, detail := languageCheck(cfg, root, name)
			add(level, "language "+name, "%s (%d file(s) indexed)", detail, used[name])
		}
	}

	if failed := count(ctx, db, `SELECT count(*) FROM files WHERE status='failed'`); failed > 0 {
		add("WARN", "files", "%d file(s) failed to index; `code-index sweep` retries them", failed)
		rows, _ := db.QueryContext(ctx, `SELECT path, COALESCE(fail_reason,'') FROM files WHERE status='failed' ORDER BY path LIMIT 5`)
		if rows != nil {
			for rows.Next() {
				var p, r string
				if rows.Scan(&p, &r) == nil {
					add("WARN", "  failed", "%s: %s", p, oneLine(r, 160))
				}
			}
			rows.Close()
		}
	}
	// stale files
	frows, err := db.QueryContext(ctx, `SELECT path, COALESCE(mtime,0), COALESCE(size,0), sha256 FROM files`)
	if err == nil {
		modified, deleted := 0, 0
		for frows.Next() {
			var p, sha string
			var mt, sz int64
			if frows.Scan(&p, &mt, &sz, &sha) == nil {
				switch search.Staleness(root, p, mt, sz, sha) {
				case "modified":
					modified++
				case "deleted":
					deleted++
				}
			}
		}
		frows.Close()
		if modified+deleted > 0 {
			add("WARN", "freshness", "%d file(s) modified and %d deleted since indexing; run `code-index sweep`", modified, deleted)
		} else {
			add("ok", "freshness", "every indexed file matches the disk")
		}
	}

	switch {
	case cerr != nil:
	case !cfg.EmbedEnabled():
		add("ok", "embedder", "not configured: search uses names and full text only")
	default:
		emb, err := newEmbedder(cfg, st)
		if err != nil {
			add("FAIL", "embedder", "%v", err)
			break
		}
		hctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		herr := emb.Health(hctx)
		cancel()
		if herr != nil {
			add("WARN", "embedder", "%s %s: %v", cfg.Embed.Provider, cfg.Embed.Model, herr)
		} else {
			add("ok", "embedder", "%s %s reachable (api key: %s)", cfg.Embed.Provider, cfg.Embed.Model, provider.KeyState(cfg.Embed.APIKeyEnv))
		}
		fp, _ := st.Meta(ctx, "embed_fingerprint")
		dims, _ := st.VecDims(ctx)
		if fp != "" && fp != emb.Fingerprint() {
			add("WARN", "embeddings", "stored with %q, configured %q: vector search is off until `code-index index --embed` re-embeds", fp, emb.Fingerprint())
		} else if dims != 0 && emb.Dims() != 0 && dims != emb.Dims() {
			add("WARN", "embeddings", "index has %d dims, embedder %d", dims, emb.Dims())
		}
		pend, _ := embed.Pending(ctx, st)
		if pend > 0 {
			add("WARN", "embeddings", "%d chunk(s) pending; run `code-index index --embed`", pend)
		} else {
			add("ok", "embeddings", "no chunks pending")
		}
		capN, src := cfg.Embed.DailyCap()
		usedToday, _ := store.UsageToday(ctx, db, store.UsageEmbed, time.Now())
		if capN == 0 {
			add("ok", "usage", "%d chunk(s) embedded today (UTC), cap: %s", usedToday, src)
		} else {
			add("ok", "usage", "%d of %d chunk(s) embedded today (UTC), cap: %s", usedToday, capN, src)
		}
	}
	return printChecks(checks, out)
}

func printChecks(checks []check, out io.Writer) int {
	code := 0
	for _, c := range checks {
		fmt.Fprintf(out, "%-5s %-14s %s\n", c.level, c.name, c.detail)
		if c.level == "FAIL" {
			code = 1
		}
	}
	return code
}

func joinKeys(m map[string]bool) string {
	if len(m) == 0 {
		return "none (defaults)"
	}
	return strings.Join(sortedKeys(m), ", ")
}

// languageCheck looks up the command of every extractor of the language
// (command_env, argv[0], command_fallbacks, as the indexer resolves them). It
// returns "ok" with "name (type): path" for each, or "WARN" naming the
// extractors whose command is missing.
func languageCheck(cfg config.Config, root, lang string) (level, detail string) {
	var found, missing []string
	for _, ne := range cfg.ExtractorsFor(lang) {
		p, err := extract.ResolveCommandIn(ne.Extractor, root, os.Getenv, exec.LookPath)
		if err != nil {
			missing = append(missing, fmt.Sprintf("%s (%s): %v", ne.Name, ne.Type, err))
			continue
		}
		found = append(found, fmt.Sprintf("%s (%s): %s", ne.Name, ne.Type, p))
	}
	switch {
	case len(found)+len(missing) == 0:
		return "WARN", "no extractor configured"
	case len(missing) > 0:
		d := "missing: " + strings.Join(missing, "; ")
		if len(found) > 0 {
			d += "; found: " + strings.Join(found, "; ")
		}
		return "WARN", d
	}
	return "ok", strings.Join(found, "; ")
}
