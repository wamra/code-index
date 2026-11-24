# Agent A: additions to docs/extractors.md (fold into sections 3, 4 and 7)

docs/ is not tracked, so the changes to sections 3-4 are listed here.

## Section 3 (interface)

- `extract.Noter` (notes.go): optional interface of a Session, `Notes() []string`. The
  indexer calls it after the edge phase and adds each line as `<lang>: <line>`. The exec
  session reports `ignored N non-JSON line(s) of extractor output` this way.
- `extract.ResolveCommand(ec, getenv, lookPath)` (exec.go): the command resolution of 4.2.
  `command_env` (non-empty) is the only candidate; else argv[0] then `command_fallbacks`.
  Not found: error wrapping `ErrExtractorUnavailable`, naming what was tried.
- Exec session behaviour: `Start` resolves the command unless `Env.Exec` is set (the seam
  replaces the subprocess, nothing is resolved). One run per distinct file list, output
  reused between phases. Working directory = root; environment = os.Environ + entry env;
  `timeout_s` (own error "timed out after Ns"; a caller cancel returns `ctx.Err()`).
  A non-zero exit returns `extractor "<name>": exit status N: <last 300 characters of stderr>`
  (the **tail**, not the first 300 characters: 4.3 said "first 300" and must be changed).
- Records are matched to requested files by exact string, then cleaned path, then
  case-folded path (when unambiguous); `Record.File` is rewritten to the requested
  spelling. Records of other files are dropped; the count goes to `Env.Logf`
  (`  <name>: ignored N record(s) for files that were not requested`), not to the notes.
- `Provides()` of an exec session: `Edges` always; `Symbols` = `config.Extractor.Symbols`
  (`*bool`, nil = false at this level).
- `Version()`: `ConfiguredVersion`, else the 8-hex content hash of the first
  `{embedded:NAME}` script in argv, else "".

## Section 4 (config)

- New optional field `extractors.<name>.symbols` (bool, exec only). `Config.ExtractorsFor`
  always returns it set: when the config leaves it out it is true only when the extractor is
  the language's only extractor, false otherwise (so the built-in `powershell-ast` next to a
  language server gives edges only, and alone it also gives symbols).
- `ExtractorsFor` also fills: `extensions` (empty = all of the language's; a built-in's list is
  intersected with the language's, so a built-in never fails validation on a language with
  other extensions; a config-defined list must be a subset, else a validation error),
  `language_id` of an lsp entry (default: the language name), `timeout_s` 600,
  `startup_timeout_s` 120, `request_timeout_s` 30.
- `Resolved()`: the implicit `<lang>-lsp` extractor is made only when `command` is set and
  none of the language's named extractors is an lsp extractor (this keeps `Resolved`
  idempotent). A language without `command` gets Command/Args/Env/LanguageID/timeouts from
  its first lsp extractor so code that still reads `lc.Command` works. Built-ins are not
  copied into `Extractors`; `config` and `config init` print the built-ins a language names.
- Validation: names match `[A-Za-z0-9._-]+`; `{listFile}` only in exec argv; `version_argv`
  must contain no placeholders; exec-only fields (`timeout_s`, `symbols`) on an lsp entry and
  lsp-only fields (`language_id`, `startup_timeout_s`, `request_timeout_s`) on an exec entry
  are errors. `config.KnownPlaceholder` mirrors `extract.IsKnownPlaceholder` (config cannot
  import extract); a test in internal/extract keeps them equal.
- Import resolution in `edges.go` no longer names `.ps1`/`.psm1`/`.psd1`: a literal ending in
  one of the language's extensions is a file path; a bare module name matches a file by base
  name plus a language extension, the later-listed extension first.
- Notes: `ErrExtractorUnavailable` still gives `<lang>: <error>; skipping the AST extractor`.
