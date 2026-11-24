package extract

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"time"

	"github.com/wamra/code-index/internal/config"
	"github.com/wamra/code-index/internal/lsp"
)

// lspExtractor adapts any language server spoken to over stdio to records.
// Nothing in it is specific to a language: the server's capabilities decide
// what it provides, the entry's language_id goes into didOpen.
type lspExtractor struct {
	lang, name string
	ec         config.Extractor
}

// newLSP builds an lsp extractor. It starts nothing.
func newLSP(lang, name string, ec config.Extractor) (Extractor, error) {
	if len(ec.Argv) == 0 || ec.Argv[0] == "" {
		return nil, fmt.Errorf("extractor %q (type lsp) for language %q: argv is empty", name, lang)
	}
	return &lspExtractor{lang: lang, name: name, ec: ec}, nil
}

func (e *lspExtractor) Name() string { return e.name }

func (e *lspExtractor) Type() string { return config.ExtractorLSP }

func (e *lspExtractor) Argv() []string { return append([]string(nil), e.ec.Argv...) }

// Version is the configured version; it never starts the server.
func (e *lspExtractor) Version(ctx context.Context) string { return ConfiguredVersion(ctx, e.ec) }

// languageID is the languageId sent in didOpen.
func (e *lspExtractor) languageID() string {
	if e.ec.LanguageID != "" {
		return e.ec.LanguageID
	}
	return e.lang
}

// Start launches the server, runs the initialize handshake and returns the
// session. A missing command wraps ErrExtractorUnavailable.
func (e *lspExtractor) Start(ctx context.Context, env Env) (Session, error) {
	argv, err := ExpandArgv(e.ec.Argv, Vars{Root: env.Root})
	if err != nil {
		return nil, fmt.Errorf("extractor %q: %w", e.name, err)
	}
	command, err := ResolveCommand(e.ec, os.Getenv, exec.LookPath)
	if err != nil && env.OpenLSP == nil {
		return nil, err // wraps ErrExtractorUnavailable
	}
	if command == "" { // injected opener: let it decide
		command = argv[0]
	}
	spec := lsp.Spec{
		Command: command, Args: argv[1:], Dir: env.Root,
		StartupTimeout: time.Duration(e.ec.StartupTimeoutS) * time.Second,
		RequestTimeout: time.Duration(e.ec.RequestTimeoutS) * time.Second,
	}
	keys := make([]string, 0, len(e.ec.Env))
	for k := range e.ec.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		spec.Env = append(spec.Env, k+"="+e.ec.Env[k])
	}
	open := env.OpenLSP
	if open == nil {
		open = lsp.Start
	}
	cl, err := open(spec)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("%w: %v", ErrExtractorUnavailable, err)
		}
		return nil, err
	}
	if err := cl.Initialize(ctx, env.Root); err != nil {
		cl.Close()
		return nil, err
	}
	caps := cl.Capabilities()
	delays := EmptyRetryDelays
	if env.EmptyRetryDelays != nil {
		delays = env.EmptyRetryDelays
	}
	return &lspSession{
		cl: cl, ex: e, env: env, caps: caps,
		delays: append([]time.Duration(nil), delays...),
	}, nil
}

// EmptyRetryDelays are the waits before an lsp session re-asks for the symbols
// of a non-empty file after an empty answer (bash-language-server answers []
// until it has analysed the document). A session copies the value at Start;
// tests assign it before Start; Env.EmptyRetryDelays overrides it per run.
var EmptyRetryDelays = []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second}

// lspSession is a running language server for one language and one run.
type lspSession struct {
	cl     *lsp.Client
	ex     *lspExtractor
	env    Env
	caps   lsp.Capabilities
	delays []time.Duration
}

// Provides: symbols when the server advertises documentSymbol, edges when it
// advertises callHierarchy or references.
func (s *lspSession) Provides() Provides {
	return Provides{
		Symbols: s.caps.DocumentSymbol,
		Edges:   s.caps.CallHierarchy || s.caps.References,
	}
}

// Close shuts the server down.
func (s *lspSession) Close() error {
	s.cl.Close()
	return nil
}

func (s *lspSession) logf(format string, args ...any) {
	if s.env.Logf != nil {
		s.env.Logf(format, args...)
	}
}
