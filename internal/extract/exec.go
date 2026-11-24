package extract

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wamra/code-index/internal/config"
)

// stderrTail is how much of an extractor's stderr goes into its error message.
const stderrTail = 300

// execWaitDelay bounds the wait for a killed extractor's inherited pipes.
const execWaitDelay = 5 * time.Second

// execExtractor is the generic exec extractor: a subprocess that is given a
// list file of absolute paths and prints JSON-lines records on stdout.
type execExtractor struct {
	lang, name string
	ec         config.Extractor
}

// newExec builds an exec extractor. It starts nothing.
func newExec(lang, name string, ec config.Extractor) (Extractor, error) {
	if len(ec.Argv) == 0 || ec.Argv[0] == "" {
		return nil, fmt.Errorf("extractor %q (type exec) for language %q: argv is empty", name, lang)
	}
	ec.Argv = append([]string(nil), ec.Argv...)
	return &execExtractor{lang: lang, name: name, ec: ec}, nil
}

func (e *execExtractor) Name() string   { return e.name }
func (e *execExtractor) Type() string   { return config.ExtractorExec }
func (e *execExtractor) Argv() []string { return append([]string(nil), e.ec.Argv...) }

// Version is the configured version (ConfiguredVersion), else the content hash
// of the first {embedded:NAME} script in argv, else "".
func (e *execExtractor) Version(ctx context.Context) string {
	if v := ConfiguredVersion(ctx, e.ec); v != "" {
		return v
	}
	for _, a := range e.ec.Argv {
		for _, tok := range Placeholders(a) {
			if name, ok := embeddedName(tok); ok {
				if _, _, hash, err := embeddedFile(name); err == nil {
					return hash
				}
			}
		}
	}
	return ""
}

// ResolveCommand picks the executable of an exec extractor: when
// ec.CommandEnv names a non-empty environment variable its value is the only
// command tried (not found: an error wrapping ErrExtractorUnavailable, no
// fallback); otherwise Argv[0], then each of CommandFallbacks. getenv and
// lookPath are os.Getenv and exec.LookPath in production.
func ResolveCommand(ec config.Extractor, getenv func(string) string, lookPath func(string) (string, error)) (string, error) {
	if ec.CommandEnv != "" {
		if v := getenv(ec.CommandEnv); v != "" {
			p, err := lookPath(v)
			if err != nil {
				return "", fmt.Errorf("%w: %s=%q was not found", ErrExtractorUnavailable, ec.CommandEnv, v)
			}
			return p, nil
		}
	}
	tried := append([]string{ec.Argv[0]}, ec.CommandFallbacks...)
	for _, c := range tried {
		if p, err := lookPath(c); err == nil {
			return p, nil
		}
	}
	quoted := make([]string, len(tried))
	for i, c := range tried {
		quoted[i] = fmt.Sprintf("%q", c)
	}
	return "", fmt.Errorf("%w: none of %s is on PATH", ErrExtractorUnavailable, strings.Join(quoted, ", "))
}

// ResolveCommandIn is ResolveCommand with {root} replaced by root in argv[0]
// and in the fallbacks first, so "{root}/tools/extract" resolves.
func ResolveCommandIn(ec config.Extractor, root string, getenv func(string) string, lookPath func(string) (string, error)) (string, error) {
	ec.Argv = append([]string(nil), ec.Argv...)
	if len(ec.Argv) > 0 {
		ec.Argv[0] = strings.ReplaceAll(ec.Argv[0], PlaceholderRoot, root)
	}
	fb := make([]string, len(ec.CommandFallbacks))
	for i, c := range ec.CommandFallbacks {
		fb[i] = strings.ReplaceAll(c, PlaceholderRoot, root)
	}
	ec.CommandFallbacks = fb
	return ResolveCommand(ec, getenv, lookPath)
}

// Start opens a session. With env.Exec set the subprocess is replaced, so the
// command is not resolved; otherwise a missing command is an error wrapping
// ErrExtractorUnavailable.
func (e *execExtractor) Start(ctx context.Context, env Env) (Session, error) {
	s := &execSession{e: e, env: env, cache: map[string][]Record{}}
	if env.Exec == nil {
		cmd, err := ResolveCommandIn(e.ec, env.Root, os.Getenv, exec.LookPath)
		if err != nil {
			return nil, err
		}
		s.cmd = cmd
	}
	return s, nil
}

// execSession runs the subprocess once per distinct file list and reuses the
// parsed output (phase 1 and phase 2 over the same files cost one run).
type execSession struct {
	e     *execExtractor
	env   Env
	cmd   string
	cache map[string][]Record
	notes []string
}

// Provides: every exec session provides edges; it provides symbols only when
// the entry says so (config.Extractor.Symbols, set by Config.ExtractorsFor).
func (s *execSession) Provides() Provides {
	return Provides{Symbols: s.e.ec.Symbols != nil && *s.e.ec.Symbols, Edges: true}
}

func (s *execSession) Symbols(ctx context.Context, files []string, emit Emit) error {
	return s.serve(ctx, files, emit)
}

func (s *execSession) Edges(ctx context.Context, files []string, _ []Record, emit Emit) error {
	return s.serve(ctx, files, emit)
}

func (s *execSession) Close() error { return nil }

// Notes implements Noter.
func (s *execSession) Notes() []string { return append([]string(nil), s.notes...) }

func (s *execSession) logf(format string, args ...any) {
	if s.env.Logf != nil {
		s.env.Logf(format, args...)
	}
}

// serve runs the extractor over files and emits one file at a time.
func (s *execSession) serve(ctx context.Context, files []string, emit Emit) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	sorted := append([]string(nil), files...)
	sort.Strings(sorted)
	key := strings.Join(sorted, "\n") // the same set in another order is the same run
	recs, ok := s.cache[key]
	if !ok {
		var err error
		if recs, err = s.run(ctx, files); err != nil {
			return err
		}
		s.cache[key] = recs
	}
	byFile, dropped := groupByFile(files, recs)
	if dropped > 0 {
		s.logf("  %s: ignored %d record(s) for files that were not requested", s.e.name, dropped)
	}
	done := map[string]bool{}
	for _, f := range files {
		if done[f] {
			continue
		}
		done[f] = true
		if err := ctx.Err(); err != nil {
			return err
		}
		rs := byFile[f]
		if rs == nil {
			rs = []Record{}
		}
		if err := emit(f, rs); err != nil {
			return err
		}
	}
	return nil
}

// groupByFile assigns each record to the requested file it names (exact string,
// else cleaned path, else case-folded path when that is unambiguous) and sets
// Record.File to the requested spelling. Records of other files are dropped
// and counted.
func groupByFile(files []string, recs []Record) (map[string][]Record, int) {
	exact := map[string]string{}
	clean := map[string]string{}
	fold := map[string]string{}
	ambiguous := map[string]bool{}
	for _, f := range files {
		exact[f] = f
		c := filepath.Clean(f)
		clean[c] = f
		l := strings.ToLower(c)
		if prev, ok := fold[l]; ok && prev != f {
			ambiguous[l] = true
		}
		fold[l] = f
	}
	by := map[string][]Record{}
	dropped := 0
	for _, r := range recs {
		f, ok := exact[r.File]
		if !ok {
			c := filepath.Clean(r.File)
			if f, ok = clean[c]; !ok {
				l := strings.ToLower(c)
				if f, ok = fold[l]; !ok || ambiguous[l] {
					dropped++
					continue
				}
			}
		}
		r.File = f
		by[f] = append(by[f], r)
	}
	return by, dropped
}

// run produces the parsed records for files: through env.Exec when set, else
// by running the subprocess.
func (s *execSession) run(ctx context.Context, files []string) ([]Record, error) {
	var out []byte
	var err error
	if s.env.Exec != nil {
		out, err = s.env.Exec(ctx, s.e.name, files)
	} else {
		out, err = s.subprocess(ctx, files)
	}
	if err != nil {
		return nil, err
	}
	recs, skipped, err := ParseLines(bytes.NewReader(out))
	if err != nil {
		return nil, err
	}
	if skipped > 0 {
		s.notes = append(s.notes, fmt.Sprintf("ignored %d non-JSON line(s) of extractor output", skipped))
	}
	return recs, nil
}

func (s *execSession) subprocess(ctx context.Context, files []string) ([]byte, error) {
	ec := s.e.ec
	list, err := os.CreateTemp("", "code-index-files-*.txt")
	if err != nil {
		return nil, err
	}
	defer os.Remove(list.Name())
	var b strings.Builder
	for _, f := range files {
		b.WriteString(f)
		b.WriteByte('\n')
	}
	if _, err := list.WriteString(b.String()); err != nil {
		list.Close()
		return nil, err
	}
	if err := list.Close(); err != nil {
		return nil, err
	}
	argv, err := ExpandArgv(ec.Argv, Vars{Root: s.env.Root, ListFile: list.Name()})
	if err != nil {
		return nil, fmt.Errorf("extractor %q: %w", s.e.name, err)
	}
	timeout := time.Duration(ec.TimeoutS) * time.Second
	if timeout <= 0 {
		timeout = config.DefaultExecTimeoutS * time.Second
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(rctx, s.cmd, argv[1:]...)
	cmd.Dir = s.env.Root
	cmd.Env = os.Environ()
	keys := make([]string, 0, len(ec.Env))
	for k := range ec.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		cmd.Env = append(cmd.Env, k+"="+ec.Env[k])
	}
	cmd.WaitDelay = execWaitDelay
	var stdout bytes.Buffer
	stderr := &tailBuffer{max: stderrTail * 4}
	cmd.Stdout, cmd.Stderr = &stdout, stderr
	if err := cmd.Run(); err != nil {
		switch {
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case errors.Is(rctx.Err(), context.DeadlineExceeded):
			return nil, fmt.Errorf("extractor %q timed out after %s", s.e.name, timeout)
		}
		return nil, fmt.Errorf("extractor %q: %w: %s", s.e.name, err, stderr.tail(stderrTail))
	}
	return stdout.Bytes(), nil
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

// tail returns the last n characters (not bytes) of the trimmed text.
func (t *tailBuffer) tail(n int) string {
	r := []rune(strings.ToValidUTF8(strings.TrimSpace(string(t.buf)), ""))
	if len(r) > n {
		r = r[len(r)-n:]
	}
	return strings.TrimSpace(string(r))
}
