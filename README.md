# code-index

Semantic and identifier search over source code, built on language servers
and extractors.

`code-index` gets the symbols of every file, with signatures and docs, from the
**extractors** you configure for a language: a language server (it asks
`textDocument/documentSymbol` and `textDocument/hover`) or any program that
prints symbols and call edges as JSON lines (an `exec` extractor, such as
`wammybash` for bash). It stores them in a local SQLite
database with full-text search and, optionally, embeddings. It works with no
model at all (names, signatures and doc comments through FTS5) and gets better
when an embedder is configured.

It is the sibling of `session-index`,
which does the same for Claude Code chat sessions, and shares its SQLite stack
(`ncruces/go-sqlite3` + `sqlite-vec` through wazero) and its provider code.

Status: milestone 2 (symbols, search, call graph and traversal). Summaries,
hooks and an LSP proxy are planned.

## Install

Building from source needs Go 1.27.4 or newer. Semantic search needs Ollama
with `embeddinggemma`, or another provider (see Configure). Without one, search
falls back to names and full text.

**macOS, Linux:**

```
scripts/install.sh              # builds, installs to ~/.local/bin, writes a starter config, adds the skill and the mod, runs doctor
cd <repo> && code-index index   # index a repo
```

Flags: `--bin-dir DIR`, `--no-skill`, `--no-mod`. The installer writes the global config
only when none exists.

**Windows (PowerShell):** `scripts\install.ps1` (`-BinDir`, `-NoSkill`, `-NoMod`).

**By hand:** `go build -o code-index ./cmd/code-index`, then
`code-index config init` and `code-index install-skill`.

**Release archives:** `scripts/build-release.sh` writes archives for macOS,
Linux and Windows (amd64, arm64) with checksums into `dist/`.

`code-index install-skill [--dir SKILLS_DIR]` writes the `code-search` skill to
`~/.claude/skills/code-search/SKILL.md`; it tells Claude Code when to use
`code-index search`, `show` and the call-graph commands.

**Tools inside Claude Code:** the installer also registers this checkout as a
plugin marketplace and installs the `code-index-search` mod: `search`, `show`,
`callers`, `callees` and `impact` tools the model calls directly, and a `/code` command
([plugins/code-index-search](plugins/code-index-search/README.md)). It is
skipped when `claude` is not on PATH. Restart Claude Code afterwards.

The binary is self-contained (no CGO).

## Platforms

macOS is the development platform. Linux and Windows run `go vet` and the test
suite in CI (`.github/workflows/ci.yml`); the tests use a fake language server
and a fake embedding server. The portability measures: SQLite uses a WAL on every platform (on Windows through a patched driver,
see `go.mod`), file URIs and drive letters are handled for Windows, CRLF sources
are accepted, server commands resolve through PATH and PATHEXT (`.cmd` shims),
and paths stored in the index and shown in output always use forward slashes.

## Configure

```sh
code-index config init     # writes ~/.config/code-index/config.json
code-index config          # prints the effective configuration
```

`config init` adds a language for every tool it finds: the language servers on
`PATH` (`bash-language-server`, `gopls`, `pyright-langserver`,
`typescript-language-server`, `rust-analyzer`, `clangd`), PowerShell Editor
Services when `pwsh` is installed and a Claude Code plugin ships a
`start-pses.ps1` under `~/.claude/plugins/cache`, and `wammybash`. When
`wammybash` is on `PATH` the bash language uses it alone (its function records
give the symbols and its calls name the file they go to); a
`bash-language-server` is then left out. Edit the file for anything else.

A language lists its `extensions` and the **extractors** that serve it, and the
`extractors` section defines them:

```json
{
  "languages": {
    "powershell": { "extensions": [".ps1", ".psm1", ".psd1"], "extractor": ["pses", "powershell-ast"] },
    "bash":       { "extensions": [".sh", ".bash"], "extractor": "wammybash" }
  },
  "extractors": {
    "pses": {
      "type": "lsp",
      "argv": ["pwsh", "-NoLogo", "-NoProfile", "-File", "/path/to/start-pses.ps1"],
      "language_id": "powershell", "startup_timeout_s": 120
    },
    "wammybash": {
      "type": "exec",
      "argv": ["wammybash", "extract", "--root", "{root}", "--list-file", "{listFile}"],
      "version_argv": ["wammybash", "version"],
      "symbols": true
    }
  },
  "embed": { "provider": "ollama", "model": "embeddinggemma", "enabled": true },
  "index": { "ignore": ["node_modules", "dist"], "max_file_size": 1048576 }
}
```

The older shape still works: a language with `command`, `args`, `env`,
`language_id` and `startup_timeout_s` is a language server (the shape of a Claude
Code plugin's `.lsp.json` entry), optionally with `"extractor": "powershell-ast"`:

```json
{ "languages": { "bash": { "command": "bash-language-server", "args": ["start"],
                           "extensions": [".sh", ".bash"], "language_id": "shellscript" } } }
```

- `languages.<name>.extractor`: one name or an ordered list. Symbols come from the
  first extractor that provides them; every extractor that provides call edges
  is run and its edges are merged. `powershell-ast` is built in (see Call graph).
- `extractors.<name>`: `type` `lsp` (argv is the server command line) or `exec`
  (argv is a program that prints JSON-lines records; `{root}`, `{listFile}` and
  `{embedded:NAME}` are replaced, any other `{...}` text is literal). Optional: `env`, `extensions` (a subset of the
  language's), `version` or `version_argv` (part of the cache key: a new version
  re-indexes the language), `command_fallbacks`, `command_env`, `timeout_s`,
  `symbols` (exec: also provides symbols; default true when it is the language's
  only extractor), `language_id`, `startup_timeout_s`, `request_timeout_s`.
  `extractors/README.md` is the full reference, including how to write an exec
  extractor.
- `embed`: providers `ollama`, `openai` (any OpenAI-compatible server) and
  `bedrock`. Embeddings are off unless `enabled` is true or a provider/model is
  given. `api_key_env` names an environment variable; keys are never printed.
- `summarize`: parsed, not used yet.
- `index.ignore`: `.gitignore`-style patterns on top of the `.gitignore` files
  of the workspace. `max_file_size` is in bytes.
- Environment: `CODE_INDEX_CONFIG` (global config file), `CODE_INDEX_DB`
  (database file), `CODE_INDEX_EMBED_{PROVIDER,MODEL,BASE_URL}`,
  `CODE_INDEX_CPUPROFILE` (write a CPU profile of the run to this file; read it
  with `go tool pprof -top code-index FILE`).

### Per-repo configuration

Configuration can live in one file or several, all with the same schema. Three
places are read, each decoded as the same `config` shape:

1. the top level of the central file (`~/.config/code-index/config.json`, or
   `CODE_INDEX_CONFIG`);
2. a `repos` entry of the central file, keyed by the repo root path;
3. `<repo>/.Wammy/config.json`.

Merge order: built-in defaults, central top level, the matching `repos` entry,
the repo's own file, environment. Each step merges section by section:
`languages` per language key, `extractors` per extractor name, `embed`,
`summarize` and `index` replaced as whole sections when the file has them. Any of
the three places may be missing: central-only, repo-file-only and both work.

```json
{
  "languages": { "go": { "extensions": [".go"], "extractor": "go-lsp" } },
  "extractors": { "go-lsp": { "type": "lsp", "argv": ["gopls"] } },
  "repos": {
    "~/dev/shell-tools": {
      "trust_repo_config": true,
      "languages": { "bash": { "extensions": [".sh", ".bash"], "extractor": "wammybash" } },
      "extractors": { "wammybash": { "type": "exec", "argv": ["wammybash", "extract", "--root", "{root}", "--list-file", "{listFile}"] } },
      "index": { "ignore": ["vendor"] }
    }
  }
}
```

A `repos` entry may set `languages`, `extractors`, `index`, `embed`, `summarize`
and `trust_repo_config`, and is fully trusted (it is in your own file), so it may
define commands. Keys are absolute paths or start with `~/`; they match the repo
root by canonical path (symlinks resolved, so `/tmp/x` and `/private/tmp/x` are one
directory; no globs). Two keys for one directory are an error. `repos` inside an
entry, and `trust_repo_config` at the top level of the central file, are errors.

`code-index config` prints the effective configuration for the current repo
only, with a `_sources` map naming the file each section came from (`default`,
a path, or `<central path>#repos.<key>`) and `_repo_config`, which says whether the
repo's own file is trusted. Notes about ignored keys go to stderr (`index`,
`sweep` and `config`).

**Trust.** `<repo>/.Wammy/config.json` can arrive with a clone, so it may not make
`index` and `sweep` run commands or send source to a host it picked (a
`version_argv` runs on every index run, even when nothing changed). Unless the
matching central `repos` entry sets `trust_repo_config: true`, the repo's file is
read through an allowlist:

- kept: `languages.<name>.extensions`, `languages.<name>.extractor` (names),
  `language_id`, the timeouts, and `index`. For a language that exists in the
  central file these overlay its entry field by field, so the central command
  stays; a language that exists only in the repo file is made from these fields;
- dropped, each with a note: `extractors.<name>`, `command`/`args`/`env` of a
  language, `embed` and `summarize`.

A name left pointing at an extractor defined nowhere is a validation error that
carries the fix: define it in the central file under `repos."<root>"`, or set
`repos."<root>".trust_repo_config` to true. `repos` and `trust_repo_config` in a
repo's file are ignored with a note, so a repo cannot trust itself. With the
repo trusted, its file behaves as before: it may define extractors and commands,
so trust only repositories you have read (see `extractors/README.md`, section 4).

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
code-index index [PATH] [--embed] [--force] [--no-edges]   index a workspace (default: .)
code-index sweep [--embed] [--force] [--no-edges]          re-index changed files, drop deleted ones
code-index search QUERY [--mode auto|vector|fts|hybrid] [--kind K] [--lang L] [--path SUBSTR] [--k 10] [--json]
code-index show SYMBOL [--source] [--json]    qualified name, name or path:line
code-index callers SYMBOL [--json]            who calls SYMBOL
code-index callees SYMBOL [--json]            what SYMBOL calls (a file path: its top-level calls)
code-index impact SYMBOL [--depth 3] [--json] transitive callers
code-index path FROM TO [--json]              shortest call path
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

## Call graph

`index` and `sweep` also build call edges (skip with `--no-edges`). Every
extractor that provides edges contributes; each edge records which kind of
extractor saw it (`source`), how sure it is (`confidence`) and why (`reason`):

- **Language servers** (`lsp` extractors). For every function and method, the
  server is asked who calls it. With `callHierarchy` (`prepareCallHierarchy`,
  `incomingCalls`) each call site is an edge with confidence `exact` and reason
  `call-hierarchy`. A server with only `textDocument/references` gives edges with
  confidence `candidate` and reason `references` (a reference is not necessarily
  a call); the declaration itself is skipped. Each edge goes from the innermost
  function, method or class that contains the site, or from the file for
  top-level code. Source `lsp`.
- **Exec extractors** (a program that prints JSON-lines records; the PowerShell
  AST extractor and `wammybash` are two). They give calls, aliases, dynamic calls,
  `dot_sources` / `imports` edges between files and `invokes` edges (one file
  running another as a child process). Each call says how sure the extractor is
  (`exact`, `alias`, `candidate`) and why, and may name the file the callee is
  defined in, which tells two functions of the same name apart. Source `ast`.
  - `powershell-ast` (built in; name it in `extractor`) runs one PowerShell
    (`pwsh`, or Windows PowerShell 5.1 `powershell` when `pwsh` is absent;
    `CODE_INDEX_POWERSHELL` overrides the interpreter) over the `.ps1` and `.psm1`
    files with the PowerShell parser. It adds calls through aliases (`Set-Alias`,
    `New-Alias`, `[Alias()]`; confidence `alias`, the alias becomes a symbol of
    kind `alias` with an `alias_of` edge), dynamic calls (`& $var`,
    `Invoke-Expression`; confidence `dynamic`, the expression text is kept) and
    the file edges. It names a callee only, so a name that resolves to a workspace
    function or alias becomes an edge; Get-Process, Write-Host and other
    built-ins are not stored.
  - `wammybash` (bash) names the file of every callee and says why: `direct`,
    `alias`, `variable-constant` (`"$cmd"` where `cmd=build_release`),
    `string-eval` (`eval "build_release y"`), with confidence `exact`, `alias` or
    `candidate`. A function that exists in two files is attributed to the right
    one.
  - A missing command is skipped with one note.

When both kinds report the same call site the row has source `lsp+ast`, and the
stronger confidence wins (`exact`, then `alias`, then `candidate`), so a
PowerShell call found by the server (`candidate`) and by the AST extractor
(`exact`) reads `[lsp+ast exact]`. When an exec extractor names the callee's file,
a server edge at that site that goes elsewhere is dropped.

Edges of a language are recomputed whenever one of its files changed or was
deleted (all of them, not only the changed files: a change moves symbol ids and
who calls whom), once after `--no-edges`, and in full (the language is re-indexed)
when an extractor's command line or version changed. Chunk text gains `calls:` and
`called by:` lines; only chunks whose text changed are re-embedded.

```
$ code-index callers Get-Widget
lib.ps1:10  Invoke-Helper  [lsp+ast exact]
main.ps1:3  main.ps1 (top level)  [lsp+ast exact]
main.ps1:4  main.ps1 (top level) (via gw)  [ast alias]

$ code-index callers lib/util.sh:4        # build_release; the bare name is ambiguous
dyn.sh:4  dyn.sh (top level)  [ast exact direct]
main.sh:6  deploy  [ast exact direct]
main.sh:7  deploy (via build_release)  [ast alias alias]
main.sh:9  deploy  [ast candidate variable-constant]
main.sh:10  deploy  [ast candidate string-eval]
```

The tag is `[source confidence]`, or `[source confidence reason]` when the extractor
gave a reason.

`impact` follows callers up to `--depth` levels (default 3, cycles are followed
once); `path` prints the shortest chain of calls. `callees` accepts a file path
for the calls made at the top level of that file. `stats` shows the edge counts
by kind, source and confidence.

## Tests

```sh
go vet ./...
go test ./...
```

The tests use a fake language server (the test binary itself), recorded
extractor output and a fake Ollama HTTP server, so they need neither a real
language server, `wammybash` nor a network. Optional tests against the real tools:
`CODE_INDEX_REAL_WAMMYBASH=/path/to/wammybash go test ./cmd/code-index -run
TestRealWammybash`, `CODE_INDEX_REAL_LSP=gopls go test ./internal/extract -run
Real`. `go test ./internal/indexer -run '^$' -bench RunSynthetic -cpuprofile
cpu.out` profiles an index run over 2000 generated files.

## Smoke test

`scripts/smoke.sh` checks the consumer path on a clean Docker image that has
only Claude Code: add this repo as a marketplace, install the
`code-index-search` plugin, and, when `ANTHROPIC_API_KEY` or
`CLAUDE_CODE_OAUTH_TOKEN` is set, call the search tool through `claude -p`.

## License

MIT, see `LICENSE`.
