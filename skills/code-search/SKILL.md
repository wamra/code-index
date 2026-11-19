---
name: code-search
description: Search the symbols (functions, classes, methods, files) of the current repo with its code-index. Use when asked where something is defined, which function does X, or to find code by name or by what it does, in a repo that has .Wammy/code.sqlite (or one worth indexing), before falling back to grep.
---

# code-search

`code-index` keeps one SQLite index per repo at `<repo>/.Wammy/code.sqlite`, built from the repo's language servers: every symbol with its kind, qualified name, `path:line`, signature and doc comment. Commands find the repo by walking up from the working directory to the nearest `.git`.

## Is there an index?

If `.Wammy/code.sqlite` exists at the repo root, use it. If it does not, tell the user and offer to build one:

```
code-index index          # indexes the repo of the working directory
```

`index` needs a language server for each language, listed in `~/.config/code-index/config.json` (`code-index config init` writes entries for the servers found on PATH; `code-index config` shows the effective configuration). `code-index doctor` checks the index, the servers and the embedder.

## Search

```
code-index search "<query>" [--mode auto|vector|fts|hybrid] [--kind function|class|method|file|...] [--lang NAME] [--path SUBSTR] [--k 10] [--json]
```

- A short identifier-shaped query (`Get-Widget`, `deploy_release`, `Widgets/Widgets.psm1`) goes to a trigram index over names and paths; the definition ranks first.
- A plain-language query ("function that returns a widget by name") goes to the vector index when embeddings exist, and to full-text search over symbol text otherwise; the last line `route:` says which. `--mode fts` skips the embedder.
- Each hit shows kind, qualified name, `path:line`, signature. `[stale: modified]` or `[stale: deleted]` means the file changed after it was indexed; read the file before trusting the line number.

## Look at one symbol

```
code-index show <SYMBOL> [--source] [--json]
```

`SYMBOL` is a qualified name, a name, or `path:line` (the innermost symbol on that line). An ambiguous name lists the candidates; pick one by qualified name or `path:line`. `--source` prints the symbol's lines from disk.

## Keep it fresh

There are no hooks yet. After editing files, or when hits are marked stale, run:

```
code-index sweep          # re-indexes changed files, drops deleted ones
```

Add `--embed` to (re)build embeddings when an embedder is configured.

## Limits

This version indexes definitions only: there is no callers/callees/impact command. Use grep for call sites.
