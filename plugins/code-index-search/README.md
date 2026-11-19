# code-index-search

A Claude Code mod that puts the `code-index` binary inside the session. It gives the model two tools and gives you one slash command.

- `search` tool: searches the current repo's code index (`<repo>/.Wammy/code.sqlite`) for functions, classes, methods and files. Filters: mode, kind, lang, path, k. `repo` points it at another repo.
- `show` tool: shows one symbol (kind, file and line range, signature, doc comment, children), optionally with its source lines.
- `/code <query>`: prints search hits. `/code show <symbol>` shows a symbol. `/code index` indexes or re-indexes this repo and embeds new chunks when an embedder is configured. `/code` alone prints index stats.

Setup: install the `code-index` binary and make sure it is on PATH, or set the plugin option `binary` to its full path. Semantic search needs an embedding provider in the code-index config; otherwise plain-language queries fall back to full-text search.

The model tools only read. Indexing is left to `/code index` or `code-index index`.

Install: `scripts/install.sh` in the code-index repository installs the binary and this mod. By hand: `claude plugin marketplace add <owner>/code-index` (or a local checkout path), then `claude plugin install code-index-search@code-index`. Restart Claude Code afterwards.
