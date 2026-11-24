# The lsp adapter (agent B), for docs/extractors.md

Files: `lsp.go` (extractor, Start, command resolution, session), `lsp_symbols.go`
(Symbols), `lsp_edges.go` (Edges), `lsp_text.go` (copies of the indexer's symbol
shaping). Tests: `lsp_test.go` (fake server, re-executed test binary), `lsp_real_test.go`
(gopls, `CODE_INDEX_REAL_LSP=gopls`).

## Start
- argv expanded with `ExpandArgv(Vars{Root})` (`{listFile}` is an error). Command: the
  value of `command_env` when that variable is non-empty (only that), else `argv[0]` then
  `command_fallbacks`, each looked up with `lsp.ResolveCommand`. None found, or the opener
  returned `exec.ErrNotFound`: error wrapping `ErrExtractorUnavailable`. When `Env.OpenLSP`
  is set and nothing resolves, the opener still gets `argv[0]` and decides.
- `lsp.Spec`: Dir = root, Env = sorted `K=V` of the entry, timeouts from `startup_timeout_s` /
  `request_timeout_s` (0 = lsp defaults). `didOpen` languageId = `language_id`, else the language.
- `Provides`: Symbols = documentSymbol advertised; Edges = callHierarchy or references advertised.
  Unlike `startServer`, a server without documentSymbol is not an error here.
- Empty-answer retry delays: package var `extract.EmptyRetryDelays` (copied at Start).
  The indexer's `Options.EmptyRetryDelays` must be assigned to it, or the merge wave adds
  `Env.EmptyRetryDelays`.

## Symbols
Per file: read, `ToValidUTF8`, strip BOM, didOpen (text without CR stripping, as today),
documentSymbol with the indexer's retry/adaptivity, `normalizeNames`, flatten, `locateName`,
hover for `hoverKinds` when advertised, didClose. Record: `t` symbol, `name`, `kind`,
`line/col/endLine/endCol` (whole range), `selLine/selCol`, `qualified` (always set, as
flatten builds it), `detail`, `parent`. `signature`/`doc` only from hover; the indexer's
`recordSymbols` fills the rest (detail, declaration line, leading comment, help block,
body hash), so the adapter does not. One difference from `indexFile`: a hover that fails (not
dead) skips the source fallbacks there, here `recordSymbols` applies them.
Errors: unreadable file or documentSymbol failure = one `error` record for the file; `ErrClosed`,
didOpen failure, ctx errors = returned.

## Edges
Targets: `known` records of kind function/method/constructor, at (`selLine`,`selCol`) else
(`line`,`col`). The file is opened per target file. With callHierarchy: prepareCallHierarchy,
then incomingCalls for each item; every `fromRange` start is a call record in `from.uri`'s file
(`exact`, `call-hierarchy`). Else references (declaration skipped): `candidate`, `references`.
Records carry `name` = target name, `to` = the known record's file, `toLine/toCol` = its name
position, no `in`. Records are grouped by file, sorted by position, deduplicated, emitted once per
requested file (empty list when none); sites in other files are dropped. Everything is emitted at
the end, so a dead server (returned) emits nothing.
Failed requests (per target) are counted: one `Env.Logf` line `  <lang>: N of M <what> request(s)
failed (first: err)` and one `error` record on the target file of the first failure with the same
text (the indexer turns it into a note). A progress line `  <lang>: N <what> request(s)` is logged.
