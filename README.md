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

Building from source needs Go 1.24.1 or newer. Semantic search needs Ollama
with `embeddinggemma`, or another provider (see Configure). Without one, search
falls back to names and full text.

**macOS, Linux:**

```
scripts/install.sh              # builds, installs to ~/.local/bin, writes a starter config, adds the skill, runs doctor
cd <repo> && code-index index   # index a repo
```

Flags: `--bin-dir DIR`, `--no-skill`. The installer writes the global config
only when none exists.

**Windows (PowerShell):** `scripts\install.ps1` (`-BinDir`, `-NoSkill`).

**By hand:** `go build -o code-index ./cmd/code-index`, then
`code-index config init` and `code-index install-skill`.

**Release archives:** `scripts/build-release.sh` writes archives for macOS,
Linux and Windows (amd64, arm64) with checksums into `dist/`.

`code-index install-skill [--dir SKILLS_DIR]` writes the `code-search` skill to
`~/.claude/skills/code-search/SKILL.md`; it tells Claude Code when to use
`code-index search` and `show`.

The binary is self-contained (no CGO).

## Platforms

macOS is the development platform and is tested. Linux and Windows are
build-verified only (`CGO_ENABLED=0` cross-builds and `go vet` for both) and
untested at runtime until CI runs: the repository has no remote yet, so
`.github/workflows/ci.yml` (vet and test on Linux and Windows) has not run. The
portability measures: SQLite uses a rollback journal on Windows and WAL
elsewhere, file URIs and drive letters are handled for Windows, CRLF sources
are accepted, server commands resolve through PATH and PATHEXT (`.cmd` shims),
and paths stored in the index and shown in output always use forward slashes.

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
- Environment: `CODE_INDEX_CONFIG` (global config file), `CODE_INDEX_DB`
  (database file), `CODE_INDEX_EMBED_{PROVIDER,MODEL,BASE_URL}`.

### Per-repo override

`<repo>/.Wammy/config.json` is merged over the global file section by section:
`languages` per language key; `embed`, `summarize` and `index` are replaced as
whole sections when the repo file has them. `code-index config` prints the
effective configuration with a `_sources` map naming the file each section came
from (or `default`).

## Where the index lives

Each repo has its own database at `<repo>/.Wammy/code.sqlite`. The repo root is
the nearest ancestor of the target directory (the `PATH` argument, else the
working directory) that contains `.git` (a directory, or a file in worktrees
and submodules); outside a git repo it is the directory itself. Every command
finds the database by walking up from the working directory, so `search` works
from any subdirectory. `CODE_INDEX_DB` overrides the location.

- `.Wammy/` is created with mode 0700 and never indexed.
- In a git repo, `index` appends `.Wammy/` to the repo's `info/exclude` (the
  real git dir, also for worktrees) unless git already ignores it, and prints a
  line when it does. `.gitignore` is never edited. `doctor` reports whether
  `.Wammy/` is ignored.
- The WAL is checkpointed on exit, so `.Wammy` holds the database file alone
  (plus `-wal`/`-shm` while a command runs) and a cloud-synced repo is safe.
  Lock files sit next to it. The wasm compile cache lives in the user cache
  directory (`code-index/wazero`), not in the repo.

## Commands

```
code-index index [PATH] [--embed] [--force]   index a workspace (default: .)
code-index sweep [--embed] [--force]          re-index changed files, drop deleted ones
code-index search QUERY [--mode auto|vector|fts|hybrid] [--kind K] [--lang L] [--path SUBSTR] [--k 10] [--json]
code-index show SYMBOL [--source] [--json]    qualified name, name or path:line
code-index stats
code-index doctor
code-index config [init [--force]]
code-index install-skill [--dir SKILLS_DIR]
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
