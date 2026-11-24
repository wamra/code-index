package extract

import (
	"context"
	"errors"
	"time"

	"github.com/wamra/code-index/internal/lsp"
)

// ErrExtractorUnavailable means the command an extractor needs is not
// installed. The indexer adds one note for the language and carries on without
// that extractor. Return it (wrapped, with the detail) from Extractor.Start.
var ErrExtractorUnavailable = errors.New("extractor interpreter not found")

// ExecFunc runs the exec extractor called name over the absolute file paths and
// returns its JSON-lines output. It is the seam tests use to serve recorded
// output: when Env.Exec is set, an exec extractor calls it instead of starting
// its subprocess. indexer.Options.Extract has this type.
type ExecFunc func(ctx context.Context, name string, files []string) ([]byte, error)

// Env is what an Extractor needs from the indexer to start a Session.
type Env struct {
	// Root is the absolute workspace root.
	Root string
	// OpenLSP starts a language server process; nil means lsp.Start. It is
	// indexer.Options.Open, kept so tests can inject the fake server.
	OpenLSP func(lsp.Spec) (*lsp.Client, error)
	// Exec, when not nil, replaces an exec extractor's subprocess (see ExecFunc).
	Exec ExecFunc
	// EmptyRetryDelays overrides the waits an lsp session makes before re-asking
	// for symbols after an empty answer (nil = the package variable
	// EmptyRetryDelays). It is indexer.Options.EmptyRetryDelays.
	EmptyRetryDelays []time.Duration
	// Logf logs one progress line; nil discards. It is the run's log, so lines
	// should be short and indented like the indexer's own ("  <what>").
	Logf func(format string, args ...any)
}

// Provides says which phases a Session can serve.
type Provides struct {
	// Symbols: the session can list the symbols of a file (phase 1).
	Symbols bool
	// Edges: the session can list calls, aliases, dotsources and so on (phase 2).
	Edges bool
}

// Emit hands the complete records of one file to the indexer. Every record has
// Record.File equal to file (the absolute path); the indexer commits the file
// before Emit returns, so a later failure does not lose it. A non-nil return
// (the indexer could not store the file, or the run was cancelled) must stop
// the session's work and be returned as is.
type Emit func(file string, recs []Record) error

// Extractor is a configured way to get symbols and edges for one language:
// an exec subprocess or an LSP server. It is cheap to create and holds no
// process; the factory New and every method except Start must not start a
// language server or run a subprocess other than the short version command.
//
// Lifecycle, per language per index run:
//
//  1. the indexer builds the language's extractors with New, in config order;
//  2. it computes the cache fingerprint from Name, Type, Argv and Version
//     (see Fingerprint), which starts nothing;
//  3. if the language has work, it calls Start once per extractor and keeps
//     the Session for both phases, then calls Close once;
//  4. phase 1 "symbols": Session.Symbols for the files that changed, on the
//     first session (in config order) whose Provides().Symbols is true;
//  5. phase 2 "edges": Session.Edges, for every file of the language, on every
//     session whose Provides().Edges is true, in config order.
type Extractor interface {
	// Name is the key of the extractor in the config ("pses").
	Name() string
	// Type is config.ExtractorExec or config.ExtractorLSP.
	Type() string
	// Argv is the configured argv template, unexpanded (use IdentityArgv for
	// the fingerprint).
	Argv() []string
	// Version returns a cheap identity string for the cache key: the static
	// "version" of the config, else the first line of the "version_argv"
	// command (ConfiguredVersion), else a hash of the embedded script; "" when
	// unknown. It must not start a language server.
	Version(ctx context.Context) string
	// Start opens a session. A missing command returns an error wrapping
	// ErrExtractorUnavailable. Starting a language server and its
	// initialize handshake happen here, not in New or Version.
	Start(ctx context.Context, env Env) (Session, error)
}

// Session is an Extractor running for one language in one index run. An exec
// session may run its subprocess once per distinct file list and reuse the
// output; an lsp session keeps one server process up across both phases. A
// session is used by one goroutine at a time.
//
// Failure model, the same for both phases:
//
//   - A problem with one file is reported as an "error" record (File and
//     Message set) inside that file's Emit. The indexer marks the file failed
//     (status 'failed', like recordFailure) and retries it on the next run; the
//     batch goes on. A "parse_error" record is informational and never fails
//     a file. In phase 2 an "error" record is a note ("extractor failed on
//     <file>: <message>") and the file's other records are used.
//   - A dead server or subprocess, or any other error that ends the batch, is
//     the error returned from Symbols or Edges. Files already passed to Emit
//     stay committed. In phase 1 the files not yet emitted are marked failed
//     with that error and the language's edges are not computed; in phase 2 no
//     edges are written for the language and its edge fingerprint is cleared.
//   - Emit once per requested file, even when the file has no records: a file
//     with no symbols is a valid empty file. A nil return with a requested file
//     never emitted means "no result": the indexer marks that file failed
//     ("extractor returned nothing for the file"). Files may be emitted in any
//     order but each at most once.
//   - Context cancellation returns ctx.Err().
type Session interface {
	// Provides is valid right after Start returns (for an lsp session it
	// reflects the server's advertised capabilities).
	Provides() Provides
	// Symbols is phase 1. files are absolute paths of the files that changed;
	// emit receives the symbol and func records of each (plus error and
	// parse_error records) with Parent ordinals, as documented in
	// extractors/README.md. Not called when Provides().Symbols is false.
	Symbols(ctx context.Context, files []string, emit Emit) error
	// Edges is phase 2. files are the absolute paths of ALL indexed files of the
	// language that the extractor is given (its "extensions" filter applied).
	// known holds the symbol records of every indexed file of the language, as
	// built by the indexer from its symbols table, including the files phase 1
	// just re-indexed and the files this extractor never saw (see
	// extractors/README.md for the guaranteed keys). emit receives the call, alias,
	// dynamic, dotsource, import, invoke, parse_error and error records of
	// each file; records are attributed to the file of the call site.
	// Not called when Provides().Edges is false.
	Edges(ctx context.Context, files []string, known []Record, emit Emit) error
	// Close releases the process. It is called exactly once, also after an
	// error.
	Close() error
}
