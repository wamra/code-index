# code-index

Semantic and identifier search over source code, built on language servers.

`code-index` starts the language server you configure for a language, asks it
for the symbols of every file (`textDocument/documentSymbol`) and their
signatures and docs (`textDocument/hover`), and stores them in a local SQLite
database with full-text search and, optionally, embeddings. It works with no
model at all (names, signatures and doc comments through FTS5) and gets better
when an embedder is configured.

It is the sibling of `session-index`,
which does the same for Claude Code chat sessions, and shares its SQLite stack
(`ncruces/go-sqlite3` + `sqlite-vec` through wazero) and its provider code.

Status: milestone 1 (core index and search). Call graphs, summaries, hooks and
an LSP proxy are planned.

## Install

```sh
go build -o code-index ./cmd/code-index
```

Go 1.24 or later. The binary is self-contained (no CGO).

## Configure

```sh
code-index config init     # writes ~/.config/code-index/config.json
code-index config          # prints the effective configuration
```

`config init` adds an entry for every language server it finds on `PATH`
(`bash-language-server`, `gopls`, `pyright-langserver`,
`typescript-language-server`, `rust-analyzer`, `clangd`) and for PowerShell
Editor Services when `pwsh` is installed and a Claude Code plugin ships a
`start-pses.ps1` under `~/.claude/plugins/cache`. Edit the file for anything
else. A language entry has the shape of a Claude Code plugin's `.lsp.json`
entry:

```json
{
  "languages": {
    "bash": {
      "command": "bash-language-server",
      "args": ["start"],
      "extensions": [".sh", ".bash"],
      "language_id": "shellscript",
      "startup_timeout_s": 120
    }
  },
  "embed": { "provider": "ollama", "model": "embeddinggemma", "enabled": true },
  "index": { "ignore": ["node_modules", "dist"], "max_file_size": 1048576 }
}
```

- `embed`: providers `ollama`, `openai` (any OpenAI-compatible server) and
  `bedrock`. Embeddings are off unless `enabled` is true or a provider/model is
  given. `api_key_env` names an environment variable; keys are never printed.
- `summarize`: parsed, not used yet.
- `index.ignore`: `.gitignore`-style patterns on top of the `.gitignore` files
  of the workspace. `max_file_size` is in bytes.
- Environment: `CODE_INDEX_CONFIG` (config file), `CODE_INDEX_DB` (database
  file), `CODE_INDEX_EMBED_{PROVIDER,MODEL,BASE_URL}`.

The database lives outside the workspace:
`~/Library/Caches/code-index/<workspace-id>/index.sqlite` on macOS,
`$XDG_CACHE_HOME` or `~/.cache` on Linux, `%LOCALAPPDATA%` on Windows.

## Commands

```
code-index index [PATH] [--embed] [--force]   index a workspace (default: .)
code-index sweep [--embed] [--force]          re-index changed files, drop deleted ones
code-index search QUERY [--mode auto|vector|fts|hybrid] [--kind K] [--lang L] [--path SUBSTR] [--k 10] [--json]
code-index show SYMBOL [--source] [--json]    qualified name, name or path:line
code-index stats
code-index doctor
code-index config [init [--force]]
code-index version
```

`search` routes like session-index: a short identifier-shaped query (a name, a
path, `Verb-Noun`, `snake_case`, `camelCase`) goes to a trigram index over
names and paths; anything else goes to the vector index when embeddings exist,
and to full-text search over the symbol text otherwise. Each hit shows kind,
qualified name, `path:line`, signature, and a `[stale: ...]` marker when the
file changed since it was indexed.

## Tests

```sh
go vet ./...
go test ./...
```

The tests use a fake language server (the test binary itself) and a fake
Ollama HTTP server, so they need neither a real language server nor a network.

## License

MIT, see `LICENSE`.
