---
name: code-search
description: Search the symbols (functions, classes, methods, files) of the current repo with its code-index, and walk its call graph (callers, callees, impact, call path). Use when asked where something is defined, which function does X, who calls a function, what breaks if it changes, or to find code by name or by what it does, in a repo that has .Wammy/code.sqlite (or one worth indexing), before falling back to grep.
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

## Call graph

```
code-index callers SYMBOL [--json]          who calls SYMBOL
code-index callees SYMBOL [--json]          what SYMBOL calls (a file path: its top-level calls)
code-index impact SYMBOL [--depth 3] [--json]   transitive callers; d1 = direct callers
code-index path FROM TO [--json]            shortest call path from FROM to TO
```

`SYMBOL` resolves as in `show`. One line per call site: `path:line  name  [source confidence]` (impact and path also show `caller -> callee`). Use `impact` before changing a function and `callers` instead of grepping for its name.

- Source: `lsp` (language server references), `ast` (the PowerShell parser), `lsp+ast` (both agree).
- Confidence: `exact` is a resolved call; `alias` went through an alias (`via gw`); `dynamic` is `& $var` / `Invoke-Expression`, only the expression text is known; `unresolved` is an alias whose target is not in the repo.
- An empty answer means no edge was recorded, not that nothing calls it: dynamic calls, other languages and files the server did not analyze are invisible. Check `code-index stats` for edge counts, and grep when it matters.
- Built-in cmdlets and external commands are not part of the graph.

## Keep it fresh

There are no hooks yet. After editing files, or when hits are marked stale, run:

```
code-index sweep          # re-indexes changed files, drops deleted ones
```

Add `--embed` to (re)build embeddings when an embedder is configured.

## Limits

- Edges come from `textDocument/references` (one request per function, so indexing a large repo takes a while) and, for PowerShell, the AST extractor (needs `pwsh`; skipped with a note otherwise). Pass `--no-edges` to `index` or `sweep` to skip them.
- Edges are recomputed for a whole language whenever one of its files changes.
- References are mapped to the innermost function, method or class that contains them; a reference in top-level code has the file as caller.
- A server may report references that are not calls (an export list, a string); they appear as `lsp` edges.
