# code-index-search

A Claude Code mod that puts the `code-index` binary inside the session. It gives the model five tools and gives you one slash command.

- `search` tool: searches the current repo's code index (`<repo>/.Wammy/code.sqlite`) for functions, classes, methods and files. Filters: mode, kind, lang, path, k. `repo` points it at another repo.
- `show` tool: shows one symbol (kind, file and line range, signature, doc comment, children), optionally with its source lines.
- `callers` tool: who calls a symbol, from the call graph in the index. One line per call site: `path:line`, the calling function (the file path for top-level code) and `[source confidence]`. Source is `lsp`, `ast` or `lsp+ast`; confidence is `exact` (a resolved call), `alias` (called through an alias, shown as `via <alias>`), `dynamic` (`& $var`, `Invoke-Expression`: the target is only known at run time) or `unresolved`.
- `callees` tool: what a function calls, in the same format. A file path lists the calls made at the top level of that file.
- `impact` tool: the transitive callers of a symbol (`depth` 1 to 10, default 3; `d1` lines are direct callers). Cycles are followed once.
- `/code <query>`: prints search hits. `/code show <symbol>` shows a symbol. `/code callers <symbol>` lists callers. `/code index` indexes or re-indexes this repo and embeds new chunks when an embedder is configured. `/code` alone prints index stats.

Setup: install the `code-index` binary and make sure it is on PATH, or set the plugin option `binary` to its full path. Semantic search needs an embedding provider in the code-index config; otherwise plain-language queries fall back to full-text search.

The model tools only read and take a `repo` parameter like `search`. The call graph exists once `code-index index` has run with edges on (the default; `--no-edges` skips them). Indexing is left to `/code index` or `code-index index`.

Install: `scripts/install.sh` in the code-index repository installs the binary and this mod. By hand: `claude plugin marketplace add <owner>/code-index` (or a local checkout path), then `claude plugin install code-index-search@code-index`. Restart Claude Code afterwards.

The `plugin` branch (built by CI from `main`) carries the six binaries, so installing from that branch needs no separate binary. `scripts/build-plugin.sh OUT` builds the same tree locally.
