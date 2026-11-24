package indexer

import "github.com/wamra/code-index/internal/extract"

// ErrExtractorUnavailable means the interpreter an extractor needs is missing.
// It is extract.ErrExtractorUnavailable, so errors.Is matches either name.
var ErrExtractorUnavailable = extract.ErrExtractorUnavailable

// ExtractFunc runs the named extractor over absolute file paths and returns
// its JSON-lines output. Options.Extract has this type and replaces the
// subprocess of every exec extractor (see extract.Env.Exec).
type ExtractFunc = extract.ExecFunc
