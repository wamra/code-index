# Extractors

code-index gets the symbols and call edges of a language from one or more
**extractors**. An extractor is either

- `exec`: a subprocess that prints JSON-lines records on stdout, or
- `lsp`: a language server spoken to over stdio, adapted to the same records.

Nothing in the indexer is specific to a language. Which extractors a language
uses is set in `config.json` (section 4). Everything an extractor says travels
as **records** (schema version 2, section 1); the indexer stores them as symbols
and edges.

Where the code is: `internal/extract` (records, interface, exec and lsp
extractors, fingerprint, argv helpers), `internal/config` (config types, loader,
validation, `Discover`), `internal/indexer` (`Run`, `sessions.go`, `records.go`,
`edges.go`), `internal/store/schema.sql` (storage). This directory holds the
embedded PowerShell script (`powershell-ast.ps1`).

Conventions that hold everywhere:

- Lines and columns in records are **1-based**. Columns count **UTF-16 code
  units** (what LSP and the .NET parser report). The database stores zero-based
  positions; the indexer converts.
- Every path in a record (`file`, `path`, `to`) is **absolute**, in native
  separators, in the workspace's own spelling. On macOS the workspace root is
  canonical (symlinks resolved, `/private/tmp`, not `/tmp`); an extractor that
  prints canonical paths matches.
- Unknown JSON keys are ignored; a known key with the wrong JSON type makes the
  line skipped (counted as non-JSON).

## 1. Record schema v2

One JSON object per line, UTF-8, `\n`-terminated. `extract.ParseLines` accepts a
line when, after trimming, it starts with `{`, decodes into `extract.Record` and
has a non-empty `t`. Any other line (a warning on stdout, for one) is counted and
skipped; the indexer notes "ignored N non-JSON line(s) of extractor output".
An exec extractor whose output has no record at all but at least one such line (a
usage message, a panic) fails the run: the error names the extractor and quotes
the first skipped line (truncated), so its files are marked failed and retried and
the language's edges are not marked computed. Output with no line at all is a valid
empty result, and output with records plus noise keeps the note.
Lines up to 8 MiB are read. The Go type is `extract.Record`
(`internal/extract/record.go`); constants `extract.Type*` name the record types.

### 1.1 Keys

| key | JSON type | meaning |
|---|---|---|
| `t` | string | record type (1.2); required |
| `file` | string | absolute path of the file the record is about (the call site's file for edges) |
| `line`, `col` | int | start of the record (call site, definition start, alias name, ...) |
| `endLine`, `endCol` | int | end of the definition (`symbol`, `func`, `alias`) |
| `name` | string | symbol name; callee name on `call` |
| `alias` | string | alias name (`alias`) |
| `target` | string | alias target text (`alias`) |
| `path` | string | file named by `dotsource`, `import`, `invoke` (absolute for `invoke`) |
| `expr` | string | source text of the site (`dynamic`: becomes the edge's `dst_name`) |
| `in` | string | name of the enclosing function; fallback when the position finds no container |
| `message` | string | `error` |
| `first`, `count` | string, int | `parse_error` (informational) |
| `to` | string | absolute path of the file that holds the definition a `call` goes to |
| `toLine`, `toCol` | int | optional position of the target definition's name, to tell two same-named symbols of one file apart |
| `resolution` | string | how sure the extractor is (1.3) |
| `reason` | string | free text: `direct`, `alias`, `variable-constant`, `string-eval`, `call-hierarchy`, `references`, ... Stored with the edge; shown by the CLI |
| `kind` | string | `symbol`: lower-case LSP SymbolKind name, as stored in `symbols.kind` (`function`, `method`, `class`, `variable`, ...) |
| `selLine`, `selCol` | int | `symbol`/`func`: position of the name; absent means "find the name in the first line of the definition" |
| `qualified` | string | `symbol`/`func`: qualified name; absent means parent's qualified name + "." + name |
| `signature`, `doc`, `detail` | string | `symbol`/`func`: signature, doc comment, server detail |
| `parent` | int | `symbol`/`func`: zero-based ordinal of the parent among the same file's `symbol`/`func` records in output order; absent = top level. A parent precedes its children. 0 is a valid value |

### 1.2 Record types

R = required, o = optional, - = not read.

| `t` | edge or row it produces | R keys | o keys |
|---|---|---|---|
| `symbol` | a row of `symbols` (source `extract`) | `file`, `name`, `kind`, `line`, `col` | `endLine`, `endCol`, `selLine`, `selCol`, `qualified`, `signature`, `doc`, `detail`, `parent` |
| `func` | shorthand for `symbol` of kind `function` | `file`, `name`, `line`, `col` | as `symbol`; `kind` is optional and defaults to `function` |
| `alias` | an `alias` symbol (source `ast`) and an `alias_of` edge | `file`, `alias`, `target`, `line`, `col` | `endLine`, `endCol`, `in` |
| `call` | a `calls` edge | `file`, `line`, `col`, `name`, **`resolution`** | `to`, `toLine`, `toCol`, `reason`, `in` |
| `dynamic` | a `calls` edge with confidence `dynamic`, `dst_name` = `expr` | `file`, `line`, `col`, `expr` | `in`, `reason`, `resolution` |
| `dotsource` | a `dot_sources` edge | `file`, `line`, `col`, `path` | `resolution`, `reason`, `in`, `expr` |
| `import` | an `imports` edge (PowerShell modules) | `file`, `line`, `col`, `path` | `resolution`, `reason`, `in`, `expr` |
| `invoke` | an `invokes` edge: another workspace file executed as a child process | `file`, `line`, `col`, `path` (absolute) | `resolution`, `reason`, `in`, `expr` |
| `parse_error` | nothing (informational; never fails the file) | `file` | `count`, `first` |
| `error` | the extractor failed on `file`: the file is marked failed (phase 1) or a note is added (phase 2) | `file`, `message` | - |

Any other `t` is accepted by the parser and ignored by the indexer.

Order contract: the records of one file appear in the order the extractor wants
them stored; `parent` ordinals count every `symbol`/`func` record of that file
(also one that is later skipped for lacking a name). A `parent` that is missing,
not smaller than the record's own ordinal, or names a skipped record makes the
symbol top level.

### 1.3 `resolution`

One function, `extract.NormalizeResolution(t, resolution, reason) (res string, ok bool)`
(table-tested in `internal/extract/extract_test.go`). `ok` is false only for a
`call` without a usable resolution.

| `t` | written value | normalised | notes |
|---|---|---|---|
| `call` | `exact` | `exact` | REQUIRED on `call` |
| `call` | `alias` | `alias` | |
| `call` | `candidate` | `candidate` | one of several possible targets, or a guess |
| `call` | `resolved` (reason not `alias`) | `exact` | synonym, written by wammybash |
| `call` | `resolved` with `reason` = `alias` | `alias` | synonym |
| `call` | absent, `unresolved`, `dynamic`, anything else | invalid | the record is **rejected**: not stored, counted (1.5) |
| `invoke`, `dotsource`, `import` | absent, `exact`, `resolved` | `exact` | resolve the target as before |
| `invoke`, `dotsource`, `import` | `resolved` with reason `alias` | `alias` | |
| `invoke`, `dotsource`, `import` | `alias`, `candidate` | as written | for `invoke` this is the edge's confidence; `dotsource`/`import` edges are always stored `exact` |
| `invoke`, `dotsource`, `import` | `unresolved`, or any unknown value | `unresolved` | no edge |
| `dynamic` | absent or anything | `dynamic` | optional |
| other types | - | none | |

Canonical values on `call` and `invoke` are `exact`, `alias`, `candidate`. They
are also the stored confidences (`dynamic` and `unresolved` exist on other edges).

### 1.4 Edge resolution for `call`

The indexer reads the records of a language after the symbols are stored
(`astEdges` in `edges.go`).

**`call` without `to`: name-only.** The callee is looked up by lower-cased `name`
among the language's function-like symbols (`function`, `method`,
`constructor`); the alias table (from `alias` records) decides confidence `alias`
and follows alias chains; `pickFunc` chooses among same-named functions (the
calling file's first, else the first by path). A name that matches nothing (a
built-in, an external command) gives no edge. The record's normalised resolution
then adjusts the result: `exact` and `alias` keep it (a name that is an alias
still gets confidence `alias`); `candidate` lowers `exact` or `alias` to
`candidate` (an `unresolved` alias result stays `unresolved`). `dst_name` is the
record's `name`.

**`call` with `to`: the destination is named.** The destination is the callable
symbol (`function`, `method`, `constructor`) called `name` in the indexed file
`to` of the language. Never a name-only lookup. Which symbol:

1. When `toLine` is set: a candidate (same name, ignoring case) whose stored name
   position equals (`toLine`, `toCol`).
2. Otherwise, or when no candidate is at that position: the exact-case name match,
   then the case-insensitive match, first by position.

Confidence = the normalised resolution. `dst_name` = the record's `name` (the CLI
shows `(via NAME)` when the confidence is `alias`). If `to` is not an indexed file
of the language, or holds no such symbol, there is no edge and the record is
counted.

In both cases the caller (`src`) is the innermost container symbol at (`line`,
`col`), else the symbol named `in`, else the file's top level (`src` NULL).

**Other types.** `dynamic`: always a `calls` edge, confidence `dynamic`, `dst_name`
= `expr`. `dotsource`: a `dot_sources` edge when `path` names an indexed file
(relative to the importing file, else to the root; or absolute), `dst_name` = that
file's path relative to the root. A literal ending in one of the language's
extensions is a file path; a bare module name matches a file by base name plus a
language extension, the later-listed extension first. `import`: the same with
module-name rules, kind `imports`. `invoke`: an `invokes` edge stored like
`dot_sources` (`dst_name` = the target's path relative to the root, no
destination symbol); dropped when `path` is not an indexed absolute path.
`reason` is stored on every edge a record produces.

Cases the rules leave open:

| case | behaviour |
|---|---|
| name-only `call` with resolution `alias` | treated like `exact`: the alias table decides `alias` confidence |
| `toLine`/`toCol` that match no same-named callable in `to` | fall back to the name rule |
| `call` with empty `name` | skipped, not counted |
| `call` record whose `file` is not an indexed file of the language | skipped silently, not counted |
| `reason` on a record that produces no edge | ignored |
| `dotsource` of a file the extractor skipped (`unresolved`, absolute `path`) | no edge |

### 1.5 Notes the indexer adds

One per language per run unless said otherwise. They are printed as
`index: note: ...` and returned in `Result.Notes`.

| condition | note |
|---|---|
| `call` records without a valid resolution | `<lang>: ignored N call record(s) without a resolution (extractor <name> writes schema v1)` |
| `call` records with `to` that find no definition | `<lang>: N call record(s) point to a definition that is not indexed` |
| non-JSON lines (exec) | `<lang>: ignored N non-JSON line(s) of extractor output` |
| an `error` record in phase 2 (also an lsp session's failed requests) | `<lang>: extractor failed on <file>: <message>` |
| an exec command that is not installed | `<lang>: <error>; skipping the AST extractor <name>` |
| an lsp server that cannot start, when no file needs indexing | `<lang>: no language server (<error>); LSP edges skipped` |
| an lsp server without references and call hierarchy | `<lang>: extractor <name> does not advertise references or call hierarchy; no LSP edges` |
| an extractor that cannot start for another reason than a missing command, when no file needs indexing | `<lang>: extractor <name>: <error>; skipped` |
| the language's extractors cannot be started or built, when no file needs indexing | `<lang>: <error>; edges not computed` |
| phase 2 failed | `<lang>: edges not computed: <error>` |

### 1.6 Merge of edges from several extractors (`edgeSet`)

Every record edge is stored with **source `lsp`** when it came from an `lsp`
extractor and **`ast`** when it came from an `exec` extractor. Rows with the same
key (kind, caller, callee, file, line, col) merge into one row with source
`lsp+ast`. Rules, in `internal/indexer/edges.go` (`edgeSet.add`, `edgeSet.sorted`;
table-tested in `edgeset_test.go` in every add order). `hasTo` below means an exec
record that carried `to`; an lsp record is evidence, never the authority on its
site, so it is never `hasTo`.

| situation | result |
|---|---|
| an exec edge **with `to`** and an lsp edge at the same site, **same destination** | one row, source `lsp+ast`, **the exec record's confidence and reason** |
| an exec edge with `to` and an lsp edge at the same site, **different destination** | the lsp edge is **dropped**. Several `to` records at one site (several candidate definitions) are all authoritative: an lsp edge survives only if its destination is one of them |
| a **name-only** exec edge and an lsp edge with the same key | one row, source `lsp+ast`, confidence = **the stronger** (`exact` > `alias` > `candidate`); the reason of the edge that supplied the confidence; on a tie the exec reason when it has one, else the first non-empty |
| a name-only exec edge and an lsp edge to different destinations | both stay (the exec record is not authoritative) |
| two exec edges, same key | stronger confidence wins |

So a PowerShell call site found by both the language server (references,
`candidate`) and the name-only AST extractor (`exact`) reads `[lsp+ast exact]`.

## 2. Symbols from records

`symbol` and `func` records of one file become the rows a language server's
symbols become. Code: `recordSymbols` and `indexFromRecords` in
`internal/indexer/records.go`.

- A `func` is a symbol of kind `function`. `kind` is lower-cased; an empty kind
  on `symbol` becomes `unknown`.
- Positions: `line`/`col`/`endLine`/`endCol` are the whole definition; a missing
  end collapses the range to its start. `selLine`/`selCol` is the name; when
  absent the name is found in the first line of the definition, preferring a
  whole-word match (`f` in `function f`).
- Filled from the source text when the record lacks them, for the kinds that get
  hover (`hoverKinds`: function, method, class, ...): `signature` = record, else
  `detail`, else the declaration line; `doc` = record, else the comment lines
  above, else a PowerShell help block; `body_sha256` from the lines the
  definition spans.
- `symbols.source` is **`extract`**, deliberately not `ast`: every edge rebuild
  deletes the `source='ast'` symbols (the alias symbols), and unchanged files are
  not re-indexed, so `ast` symbols would vanish. Values: `extract` (from records,
  whatever the extractor type), `ast` (aliases). Nothing in the CLI reads the
  column.
- The same transaction writes the FTS and chunk rows.

`knownSymbols(ctx, db, root, lang)` builds the `known` argument of `Session.Edges`
from the symbols table: one `symbol` record per stored symbol of every `ok` file
of the language, excluding `source='ast'` aliases. Keys: `t`="symbol", absolute
`file`, `name`, `kind`, `qualified`, `line`/`col`/`endLine`/`endCol` (whole
definition), `selLine`/`selCol` (the stored name position), `signature` and `doc`
when stored, `parent` (ordinal among the same file's records in this order,
absent for top level). Order: files by path, then start position, then id.

## 3. The Extractor interface

Package `internal/extract`, file `extractor.go`:

```go
var ErrExtractorUnavailable = errors.New("extractor interpreter not found")

type ExecFunc func(ctx context.Context, name string, files []string) ([]byte, error)

type Env struct {
	Root             string                              // absolute workspace root
	OpenLSP          func(lsp.Spec) (*lsp.Client, error) // nil = lsp.Start (indexer.Options.Open)
	Exec             ExecFunc                            // when non-nil replaces an exec extractor's subprocess (indexer.Options.Extract)
	EmptyRetryDelays []time.Duration                     // lsp: waits before re-asking for symbols after an empty answer (nil = extract.EmptyRetryDelays)
	Logf             func(format string, args ...any)    // nil discards
}

type Provides struct{ Symbols, Edges bool }
type Emit func(file string, recs []Record) error

type Extractor interface {
	Name() string
	Type() string                                   // config.ExtractorExec | config.ExtractorLSP
	Argv() []string                                 // the configured template, unexpanded
	Version(ctx context.Context) string
	Start(ctx context.Context, env Env) (Session, error)
}

type Session interface {
	Provides() Provides
	Symbols(ctx context.Context, files []string, emit Emit) error
	Edges(ctx context.Context, files []string, known []Record, emit Emit) error
	Close() error
}

func New(lang, name string, ec config.Extractor) (Extractor, error)
```

`New` switches on `ec.Type` (`exec` -> `newExec`, `lsp` -> `newLSP`); anything else
is an error. A session may also implement `extract.Noter` (`Notes() []string`);
the indexer adds each line as `<lang>: <line>` after the edge phase.

### 3.1 Lifecycle, per language per index run

1. `extract.New(lang, name, ec)` for each extractor of the language
   (`Config.ExtractorsFor`, config order). It starts nothing.
2. The cache fingerprint is computed (section 5). It runs at most the short
   `version_argv` command, never a language server.
3. If the language has work (changed files, or edges to rebuild), `Start` is
   called once per extractor and the `Session` is kept for both phases, then
   `Close` once, also after errors. When no edges are to be rebuilt, extractors
   after the first one that provides symbols are not started. `Start` returns an
   error wrapping `ErrExtractorUnavailable` when the command is missing (the
   extractor is left out with a note). Any other `Start` error is a language
   error when files need indexing, a note otherwise. An lsp session does its
   `initialize` handshake in `Start`; `Provides()` reflects the server's
   capabilities. An exec session does nothing in `Start` except resolve its
   command.
4. **Phase 1, symbols**: the changed files go to the first session in config
   order whose `Provides().Symbols` is true. If no started session provides
   symbols the language is unusable for the run: every changed file counts as
   failed and `Result.LangErrors` says why (`<command> does not advertise
   documentSymbol` for a language server, `extractor <name> does not provide
   symbols` for an exec entry). A changed file outside that extractor's
   `extensions` is stored without symbols.
5. **Phase 2, edges**: every session whose `Provides().Edges` is true, in config
   order, gets all indexed files of the language that its `extensions` select,
   and `known` (`knownSymbols`: the symbols of every indexed file, including the
   ones phase 1 just stored). That lets an lsp session ask for references and
   call hierarchy at name positions without a documentSymbol request per unchanged
   file. The records of all sessions are merged by `edgeSet` and the language's
   edges are rebuilt in one transaction.
6. An exec session runs its subprocess once per distinct set of files (the order
   does not matter) and reuses the output between phases, so the first index of a
   language costs one run. An lsp session keeps one server up across both phases.

### 3.2 Emit: one file at a time

`Emit(file, recs)` hands over the complete records of one file; every record has
`File == file`. The indexer commits the file before `Emit` returns (phase 1 logs
`  path: N symbol(s)`), so a crash later does not lose it. A non-nil return from
`Emit` (store failure, cancelled run) must stop the session's work and be returned
as it is. In phase 2 the records of all files are collected and the edges are
written at the end.

Rules for a session:

- Call `Emit` **once per requested file**, in any order, even when the file has no
  records (a file with no symbols is a valid empty file; an exec adapter emits an
  empty list for every requested file it saw no records for after the subprocess
  exits successfully).
- A nil return with a requested file never emitted means "no result": the file is
  marked failed ("extractor returned nothing for the file").
- Per-file failure: an `error` record (`file`, `message`) in that file's `Emit`.
  Phase 1: the file is marked `failed` (status in `files`, retried next run), the
  batch goes on. Phase 2: a note, and the file's other records are still used. A
  `parse_error` record is informational and never fails a file.
- A dead server or subprocess, or any error that ends the batch, is the returned
  error. Files already emitted stay committed. Phase 1: files not yet emitted are
  marked failed with that error, the language is reported in `LangErrors` and its
  edges are not computed. Phase 2: no edges are written for the language and its
  fingerprint marker is cleared (retry next run). `ErrExtractorUnavailable` from
  `Edges` is a note and that session's records are dropped.
- Context cancellation returns `ctx.Err()`; the files not reached stay as they were.

### 3.3 Helpers

| function | file | what |
|---|---|---|
| `extract.ExpandArgv(argv, Vars{Root, ListFile, Embedded})` | `argv.go` | expands `{root}`, `{listFile}`, `{embedded:NAME}`; a `{listFile}` without a list file or an `{embedded:NAME}` with an unknown NAME is an error; other `{...}` text is left as it is |
| `extract.EmbeddedPath(name)` | `argv.go` | writes the embedded file to `<user cache dir>/code-index/extractors/<stem>-<hash8><ext>` and returns the path; the lookup table is `extractors.Embedded(name)` |
| `extract.IdentityArgv(argv, root)` | `argv.go` | the argv used in the fingerprint (section 5) |
| `extract.ResolveCommand(ec, getenv, lookPath)` | `exec.go` | command resolution (4.2), shared by exec and lsp extractors and by `doctor` |
| `extract.ConfiguredVersion(ctx, ec)` | `version.go` | `ec.Version`, else the first stdout line of `ec.VersionArgv` (10 s, `ec.Env`), else `""`; a missing or failing command gives `""` |
| `extract.NormalizeResolution`, `extract.Strength` | `resolution.go` | 1.3 |
| `indexer.recordSymbols`, `indexFromRecords`, `knownSymbols` | `indexer/records.go` | section 2 |

`indexer.ExtractFunc` is an alias of `extract.ExecFunc`, and
`indexer.ErrExtractorUnavailable` is `extract.ErrExtractorUnavailable`.

### 3.4 The exec session

- `Start` resolves the command (`ResolveCommand`) unless `Env.Exec` is set (the seam
  replaces the subprocess; nothing is resolved).
- The subprocess runs with the working directory set to the root, the environment
  of code-index plus the entry's `env`, and `timeout_s` (error "timed out after
  Ns"; a caller cancel returns `ctx.Err()`). `{listFile}` is a temp file with one
  absolute path per line, each `\n`-terminated.
- On unix the subprocess (and the `version_argv` command) is started in its own
  process group and a timeout or cancel kills the whole group, so grandchildren do
  not survive; a 2 s `WaitDelay` is the backstop for inherited pipes. **On Windows
  only the direct child is killed**: a grandchild it started keeps running, and a
  grandchild that holds the stdout pipe open can delay the return by that delay.
- An extractor that exits 0 but leaves a process holding stdout (a daemon) is not a
  failure: when the 2 s `WaitDelay` expires after a successful exit, the output
  gathered so far is used, the process group is killed (unix) and one run-log line
  says so.
- `code-index index` and `sweep` stop on SIGINT, and on unix also on SIGTERM and
  SIGHUP (terminal close, `timeout`, CI cancel): the run cancels its context, which
  kills the extractor's process group, then exits non-zero; the lock is released and
  the files not reached stay retryable. Without this the separate process group
  would leave the extractor running until `timeout_s`. A SIGHUP that was inherited
  as ignored (`nohup`, `disown`) stays ignored, so a detached run survives its
  terminal. Windows handles the
  interrupt only.
- A non-zero exit returns `extractor "<name>": exit status N: <last 300 characters of stderr>`.
- Records are matched to requested files by exact string, then cleaned path, then
  case-folded path (when unambiguous); `Record.File` is rewritten to the requested
  spelling. Records of other files are dropped and counted in the run log.
- `Provides()`: `Edges` always; `Symbols` = `config.Extractor.Symbols`.

### 3.5 The lsp adapter

Files: `lsp.go` (extractor, `Start`, session), `lsp_symbols.go`, `lsp_edges.go`,
`lsp_text.go`. Tests use the fake server in `internal/lsptest`; `lsp_real_test.go`
runs gopls when `CODE_INDEX_REAL_LSP=gopls`.

- **Start**: argv expanded with `{root}` (`{listFile}` is an error); command
  resolved with `ResolveCommand`; `lsp.Spec` has `Dir` = root, the entry's `env`
  (sorted `K=V`) and its timeouts (0 = lsp defaults); `didOpen` sends
  `language_id` (default: the language name). A missing command, or an opener that
  returned `exec.ErrNotFound`, wraps `ErrExtractorUnavailable`. With an injected
  `Env.OpenLSP` and nothing resolving, the opener still gets `argv[0]` and decides.
- **Provides**: `Symbols` = `documentSymbol` advertised; `Edges` = `callHierarchy`
  or `references` advertised.
- **Symbols**, per file: read, replace invalid UTF-8, strip the BOM, `didOpen`,
  `documentSymbol` (an empty answer for a non-empty file is asked again after
  `EmptyRetryDelays`, because bash-language-server answers `[]` until it has
  analysed the document; retries stop after three that found nothing, stay on
  once one helped), normalise names, flatten the hierarchy (or a flat
  `SymbolInformation` list by range containment), locate the name, `hover` for the
  hover kinds when advertised, `didClose`. Record: `t` symbol, `name`, `kind`,
  `line/col/endLine/endCol` (whole range), `selLine/selCol`, `qualified`, `detail`,
  `parent`; `signature`/`doc` only from hover (the indexer fills the rest from
  `detail` and the source text). An unreadable file or a failing `documentSymbol`
  is one `error` record; `ErrClosed`, a `didOpen` failure and context errors are
  returned.
- **Edges**: targets are the `known` records of kind function, method or
  constructor, at their name position. With `callHierarchy`: `prepareCallHierarchy`
  then `incomingCalls`; every `fromRange` start is a `call` record in the file of
  `from.uri`, resolution `exact`, reason `call-hierarchy`. Without it, `references`
  (declaration skipped): resolution `candidate`, reason `references` (a reference
  is not necessarily a call). Records carry `name` = the target's name, `to` = its
  file, `toLine/toCol` = its name position, no `in` (the indexer finds the
  container). Records are grouped by file, sorted by position, de-duplicated,
  emitted once per requested file; sites in other files are dropped. A failing
  request is counted: one run-log line and one `error` record (a note) with
  `N of M <what> request(s) failed (first: ...)`. A dead server is returned and
  nothing is emitted.

### 3.6 Writing an exec extractor

Any program that reads a list of absolute paths and prints one JSON object per
line works. This one (shell, 8 lines; run once through `index`, it stored the
two functions of a test file as symbols) prints a `func` record for every
`name()` line of every listed file:

```sh
#!/bin/sh
# usage: extract.sh --root DIR --list-file FILE
while read -r f; do
  grep -n '^[A-Za-z_][A-Za-z_0-9]*()' "$f" | while IFS=: read -r n rest; do
    name=${rest%%(*}
    printf '{"t":"func","file":"%s","name":"%s","line":%s,"col":1}\n' "$f" "$name" "$n"
  done
done < "$4"
```

Configure it with `"extractors": {"mine": {"type": "exec", "argv": ["/path/extract.sh",
"--root", "{root}", "--list-file", "{listFile}"], "symbols": true}}` and name it in
a language. It gives symbols only. Add `call` records (`file`, `line`, `col`,
`name`, `resolution`, and `to` when you know the file) for edges, `error` records for files you cannot
read, and a `version_argv` so a new version of your tool re-indexes.

## 4. Config schema

```json
{
  "languages": {
    "powershell": { "extensions": [".ps1", ".psm1", ".psd1"], "extractor": ["pses", "powershell-ast"] },
    "bash":       { "extensions": [".sh", ".bash"], "extractor": "wammybash" }
  },
  "extractors": {
    "pses":           { "type": "lsp",  "argv": ["pwsh", "-NoLogo", "-NoProfile", "-File", "/path/start-pses.ps1"], "language_id": "powershell", "startup_timeout_s": 120 },
    "powershell-ast": { "type": "exec", "argv": ["pwsh", "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", "{embedded:powershell-ast.ps1}", "-ListFile", "{listFile}"], "command_fallbacks": ["powershell"], "command_env": "CODE_INDEX_POWERSHELL", "extensions": [".ps1", ".psm1"] },
    "wammybash":      { "type": "exec", "argv": ["wammybash", "extract", "--root", "{root}", "--list-file", "{listFile}"], "version_argv": ["wammybash", "version"], "symbols": true }
  }
}
```

(`TestExtractorsConfigExampleDecodes` decodes this example without the `symbols`
key; the decode keeps `DisallowUnknownFields`.)

### 4.1 `languages.<name>`

| field | type | meaning |
|---|---|---|
| `extensions` | string[] | file extensions (with dot, case-insensitive) that belong to the language; required |
| `extractor` | string or string[] | one extractor name or an ordered list; at least one. **The order is the run order.** Symbols come from the first extractor that provides symbols; every extractor that provides edges is run for phase 2 and its edges are merged |
| legacy: `command`, `args`, `env`, `language_id`, `startup_timeout_s`, `request_timeout_s` | | see 4.4 |

In Go, `config.ExtractorNames` is a string-kind type (names joined by a comma);
read it with `.Names()`, build it with `config.NewExtractorNames(...)`. Its JSON
methods accept a string or an array, write one name as a string, and reject an
empty array, an empty name and a name containing a comma. Extractor names match
`[A-Za-z0-9._-]+`.

### 4.2 `extractors.<name>`

| field | applies to | default | meaning |
|---|---|---|---|
| `type` | both | required | `exec` or `lsp` |
| `argv` | both | required | command line template; `argv[0]` is the command, resolved through PATH/PATHEXT |
| `env` | both | none | extra environment variables for the process |
| `extensions` | both | all of the language's | subset of the language's extensions this extractor is given; an extension not in the language's list is a validation error. A built-in's list is intersected with the language's instead |
| `version` | both | none | static version string for the cache key |
| `version_argv` | both | none | command whose trimmed stdout's first line is the version; 10 s timeout; no placeholders. `version` wins when both are set |
| `command_fallbacks` | both | none | other commands tried for `argv[0]`, in order, when it is not on PATH |
| `command_env` | both | none | name of an environment variable that overrides `argv[0]`. When it is set (non-empty) and the command is not found the extractor is unavailable, **no fallback** |
| `timeout_s` | exec | 600 | run timeout |
| `symbols` | exec | see below | the extractor also provides the symbols of a file (phase 1); every exec extractor provides edges |
| `language_id` | lsp | the language name | `languageId` sent in `didOpen` |
| `startup_timeout_s` | lsp | 120 | `initialize` timeout |
| `request_timeout_s` | lsp | 30 | per-request timeout |

`symbols` unset: true only when the entry is the language's only extractor
(`Config.ExtractorsFor` always returns it set). So `powershell-ast` next to a
language server gives edges only, and alone it also gives symbols. Exec-only
fields on an lsp entry and lsp-only fields on an exec entry are validation errors.

Command resolution order (`extract.ResolveCommand`): if `command_env` names a
non-empty variable, its value is the only command tried; otherwise `argv[0]`, then
each of `command_fallbacks` in order; none found means unavailable.

### Trust

`<repo>/.Wammy/config.json` can define extractors, that is commands that `index`
and `sweep` run in that repository, and a `version_argv` runs on every index run,
even when nothing changed, to compute the cache key. As before extractors existed
(a per-repo file could already set a language server command), only index
repositories whose `.Wammy/config.json` you trust. There is no trust gate.

### 4.3 Placeholders in `argv`

| placeholder | meaning |
|---|---|
| `{root}` | the absolute workspace root |
| `{listFile}` | exec only: a temp file with one absolute path per line, each `\n`-terminated (also the last) |
| `{embedded:NAME}` | path of the file `NAME` embedded in this directory's Go package (today `powershell-ast.ps1`), written to `<user cache dir>/code-index/extractors/` under a content-hash name |

A placeholder may be a whole argument or part of one (`--root={root}`). Only these three are
special. Any other `{...}` text is literal and passes through unexpanded, in
validation, in `ExpandArgv` and in the fingerprint's `IdentityArgv`: a JSON
argument (`--config={"a":1}`), an awk program (`awk '{print $1}'`) or `{}`. A brace
that is not part of a `{...}` pair is literal too. Validation errors: `{listFile}`
in an `lsp` extractor, and `{embedded:NAME}` when NAME is not an embedded file.
Because the three tokens are always expanded, there is no escape for them.

### 4.4 Built-ins, the legacy shape, discovery

`config.BuiltinExtractors()` returns the definitions that exist without a config
file: today `powershell-ast`, exactly the JSON above. A config may reference one
without defining it and may override it by defining the same name (the whole
entry is replaced). `pwsh -ExecutionPolicy Bypass` is accepted on macOS and Linux
too, so there is no per-OS argv.

The legacy language shape stays accepted: a language with `command` (and optional
`args`, `env`, `language_id`, `startup_timeout_s`, `request_timeout_s`) and
optional `"extractor": "powershell-ast"`. `Config.Resolved()` turns it into an
implicit lsp extractor named `<language>-lsp` placed first, followed by the named
extractors; it is made only when the language names no lsp extractor, so
`Resolved` is idempotent. `config` and `config init` print the new shape. The
legacy `Command`/`Args` fields on `config.Language` are filled for a new-style
language from its first lsp extractor and are read only inside `internal/config`.
`doctor` checks each extractor's command instead.

`config init` (`config.Discover`) writes the languages it finds: the known
language servers on PATH (`bash-language-server`, `gopls`, `pyright-langserver`,
`typescript-language-server`, `rust-analyzer`, `clangd`), PowerShell Editor
Services (with `powershell-ast`) when `pwsh` and a plugin's `start-pses.ps1` are
found, and wammybash. **When `wammybash` is on PATH the bash language uses
`["wammybash"]` alone**, as an exec extractor with `symbols: true`,
`version_argv: ["wammybash","version"]` and extensions `.sh`, `.bash` (it answers
`.zsh` with an error record, so `.zsh` and `.ksh` are left out), and a
bash-language-server on PATH is not configured: wammybash's calls carry `to`,
resolution and reason, and its `func` records give the symbols, which a language
server's references cannot match. Without wammybash the bash entry is
bash-language-server as before. To use both, list both extractors by hand
(`"extractor": ["bash-lsp", "wammybash"]`: symbols from the server, edges from
both).

## 5. Cache key

`meta["edges_lang:<lang>"]` holds a language's fingerprint once its edges are
computed. `extract.Fingerprint(parts []Part)` is the sha256 (hex) of the JSON of
`{schema: extract.SchemaVersion (2), extractors: [...]}`; each extractor, in the
language's run order, contributes:

| part | value |
|---|---|
| `Name` | the extractor's config name |
| `Type` | `exec` or `lsp` |
| `Argv` | `extract.IdentityArgv(argv, root)`: the template with `{root}` replaced by the root, `{embedded:NAME}` replaced by `embedded:NAME@<8-hex content hash>` (nothing is written; editing the embedded script changes the key) and `{listFile}` left as literal text |
| `Version` | `Extractor.Version(ctx)` of the built extractor: `version`, else the first line of `version_argv` (`extract.ConfiguredVersion`), else for an exec extractor the content hash of its first `{embedded:NAME}` script, else `""` |

`extract.LangParts` builds the parts (`extract.New`, then `extract.PartOf`) and
`indexer.langFingerprint` hashes them. An extractor that cannot be built or whose
argv cannot be expanded (validation rejects both) gets a part whose version is
"fingerprint unavailable: <error>", so the language is never marked up to date by
such a part.

The key does not cover `extensions`, `symbols`, `env` or the timeouts: after
changing one, run `index --force`. Computing the fingerprint starts no language
server (`TestLSPReferencesBecomeCallsEdges` asserts no server is opened on an
unchanged run).

| stored value | meaning |
|---|---|
| equals the current fingerprint | up to date |
| `""` (never computed, cleared by `--no-edges`, a failure, or a dead server) | **edges only**: rebuild the edges without re-indexing files |
| anything else (an older fingerprint, or the legacy `"1"`) | **fully stale**: all the language's files are re-indexed as with `--force` for that language, and its edges rebuilt |

A fully stale language **keeps its old marker when its run does not complete**
(a server that cannot start, a cancellation, a dead server, a failed phase), so
the next run finds it stale again and re-indexes the files this run did not reach.
Under `--no-edges` the marker becomes `""` only after the re-index finished. A
language that is up to date and has no changed file starts no language server and
runs no extractor; only a `version_argv` command, when one is configured, runs
(a second `index` run takes milliseconds).

## 6. Storage and output

- `edges.reason TEXT` (NULL when the extractor gave none). New databases get it
  from `schema.sql`; `store.Open` adds it to older databases (`ALTER TABLE ... ADD
  COLUMN` when `PRAGMA table_info(edges)` lacks it; idempotent).
- `edges.kind`: `calls | alias_of | imports | dot_sources | invokes`.
- `edges.confidence`: `exact | alias | candidate | dynamic | unresolved`.
- `edges.source`: `lsp | ast | lsp+ast`. `symbols.source`: `lsp | ast | extract`
  (new rows from records are `extract`; `lsp` is only in indexes made before).
- `callers`, `callees`, `impact`, `path`: the tag is `[source confidence]`, or
  `[source confidence reason]` when the reason is non-empty. JSON edges gain
  `"reason"` with `omitempty`. `stats` counts edges `GROUP BY kind|source|confidence`.
- `callees <file path>` lists only the `calls` edges made at the top level of the
  file; `dot_sources` and `invokes` edges are in the table and counted by `stats`.

### PowerShell extractor

`powershell-ast.ps1` writes `"resolution":"exact"` on every `call` record
(name-only; code-index derives `alias` itself). `internal/indexer/testdata/powershell-ast.sample.jsonl`
is a recording of a real run on `testdata/fixture-ps`. The script stays ASCII and
PowerShell 5.1 compatible. `TestRealPowerShellExtractor/pwsh` and `/powershell` in
`internal/indexer` are what CI greps for on Windows.

## 7. wammybash

`wammybash extract --root DIR --list-file FILE` prints v2 records for bash. Its
`call` records carry `to`, `resolution` (`resolved` or `candidate`) and `reason`
(`direct`, `alias`, `variable-constant`, `string-eval`, `string-trap`); `func`
records carry `name`, `line`, `col`, `endLine`, `endCol`; `dotsource` records
carry an absolute `path`; `invoke`, `alias`, `parse_error` and `error` as in the
tables above. `internal/extract/testdata/wammybash-extract.golden.jsonl` is its
real output on the corpus in `testdata/fixture-bash-wammybash` (`<root>/` stands
for the absolute root); the tests feed it through the `Options.Extract` seam and
through a re-executed test binary, so CI needs no wammybash.
`CODE_INDEX_REAL_WAMMYBASH=/path/to/wammybash go test ./cmd/code-index -run
TestRealWammybash` runs the real tool.

Observed limits of the format: a call through an alias is written with `name` =
the function, so `(via NAME)` in `callers` shows the function's own name rather
than the alias; an `alias` record has no `to`, so its `alias_of` edge is resolved
by name (first function of that name by path).

## 8. CPU profile

`CODE_INDEX_CPUPROFILE=<file>` makes any command write a CPU profile of its run
(`go tool pprof -top code-index <file>`).
