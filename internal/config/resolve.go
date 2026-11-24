package config

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/wamra/code-index/extractors"
)

// Defaults of the extractor fields (extractors/README.md, section 4.2).
const (
	DefaultExecTimeoutS = 600
	DefaultStartupS     = 120
	DefaultRequestS     = 30
)

// implicitLSPName is the name of the lsp extractor made from a legacy language.
func implicitLSPName(lang string) string { return lang + "-lsp" }

// lookupExtractor finds an extractor by name: the config's entry (which
// replaces a built-in of the same name as a whole), else the built-in.
func (c Config) lookupExtractor(name string) (e Extractor, builtin, ok bool) {
	if e, ok := c.Extractors[name]; ok {
		return e, false, true
	}
	if e, ok := BuiltinExtractors()[name]; ok {
		return e, true, true
	}
	return Extractor{}, false, false
}

// withDefaults fills the defaults of the fields that belong to e's type; the
// fields of the other type stay as written so that Validate can reject them.
func (e Extractor) withDefaults() Extractor {
	switch e.Type {
	case ExtractorExec:
		if e.TimeoutS <= 0 {
			e.TimeoutS = DefaultExecTimeoutS
		}
	case ExtractorLSP:
		if e.StartupTimeoutS <= 0 {
			e.StartupTimeoutS = DefaultStartupS
		}
		if e.RequestTimeoutS <= 0 {
			e.RequestTimeoutS = DefaultRequestS
		}
	}
	return e
}

// resolveExtractors normalises extractors and languages (extractors/README.md 4.4):
//   - a language with Command and no lsp extractor among its names gets an
//     implicit lsp extractor "<lang>-lsp" placed first;
//   - a language without Command gets the legacy fields of its first lsp
//     extractor, so code that reads lc.Command keeps working;
//   - defaults are filled in every configured extractor.
//
// The Extractors map is copied, never written through. Built-ins are not copied
// in. Running it twice changes nothing: after the first run a legacy language
// names an lsp extractor.
func (c Config) resolveExtractors() Config {
	exs := make(map[string]Extractor, len(c.Extractors))
	for n, e := range c.Extractors {
		exs[n] = e
	}
	c.Extractors = exs
	langs := make(map[string]Language, len(c.Languages))
	for name, l := range c.Languages {
		names := l.Extractor.Names()
		first, hasLSP := c.firstLSP(names)
		switch {
		case l.Command != "" && !hasLSP:
			imp := implicitLSPName(name)
			exs[imp] = Extractor{
				Type: ExtractorLSP, Argv: append([]string{l.Command}, l.Args...), Env: l.Env,
				LanguageID: l.LanguageID, StartupTimeoutS: l.StartupTimeoutS, RequestTimeoutS: l.RequestTimeoutS,
			}
			rest := []string{imp}
			for _, n := range names {
				if n != imp {
					rest = append(rest, n)
				}
			}
			l.Extractor = NewExtractorNames(rest...)
		case l.Command == "" && hasLSP && len(first.Argv) > 0:
			l.Command, l.Args, l.Env = first.Argv[0], append([]string(nil), first.Argv[1:]...), first.Env
			if first.LanguageID != "" {
				l.LanguageID = first.LanguageID
			}
			if first.StartupTimeoutS > 0 {
				l.StartupTimeoutS = first.StartupTimeoutS
			}
			if first.RequestTimeoutS > 0 {
				l.RequestTimeoutS = first.RequestTimeoutS
			}
		}
		langs[name] = l
	}
	c.Languages = langs
	for n, e := range exs {
		exs[n] = e.withDefaults()
	}
	return c
}

// firstLSP returns the first of names that is an lsp extractor.
func (c Config) firstLSP(names []string) (Extractor, bool) {
	for _, n := range names {
		if e, _, ok := c.lookupExtractor(n); ok && e.Type == ExtractorLSP {
			return e, true
		}
	}
	return Extractor{}, false
}

// usedExtractors is what the effective config and `config init` print: the
// configured extractors plus the built-ins some language names and the config
// does not define, all with defaults.
func (c Config) usedExtractors() map[string]Extractor {
	out := make(map[string]Extractor, len(c.Extractors))
	for n, e := range c.Extractors {
		out[n] = e.withDefaults()
	}
	for _, l := range c.Languages {
		for _, n := range l.Extractor.Names() {
			if _, have := out[n]; have {
				continue
			}
			if e, builtin, ok := c.lookupExtractor(n); ok && builtin {
				out[n] = e.withDefaults()
			}
		}
	}
	return out
}

// effectiveSources returns src plus a source for every printed extractor that
// no file defined: the language's own source for an implicit "<lang>-lsp",
// "built-in" for a built-in.
func effectiveSources(c Config, src Sources, exs map[string]Extractor) Sources {
	s := Sources{}
	for k, v := range src {
		s[k] = v
	}
	for n := range exs {
		key := "extractors." + n
		if _, ok := s[key]; ok {
			continue
		}
		if lang := strings.TrimSuffix(n, "-lsp"); lang != n {
			if v, ok := s["languages."+lang]; ok {
				s[key] = v
				continue
			}
		}
		s[key] = "built-in"
	}
	return s
}

var (
	extractorNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	placeholderRe   = regexp.MustCompile(`\{[^{}]*\}`)
)

// knownPlaceholder mirrors extract.IsKnownPlaceholder (internal/extract imports
// this package, so it cannot be called from here); a test in internal/extract
// keeps the two equal.
func knownPlaceholder(tok string) bool {
	switch tok {
	case "{root}", "{listFile}":
		return true
	}
	if strings.HasPrefix(tok, "{embedded:") && strings.HasSuffix(tok, "}") {
		_, found := extractors.Embedded(tok[len("{embedded:") : len(tok)-1])
		return found
	}
	return false
}

// KnownPlaceholder reports whether tok ("{root}", "{listFile}" or
// "{embedded:NAME}" with an existing embedded file) is a placeholder argv may use.
func KnownPlaceholder(tok string) bool { return knownPlaceholder(tok) }

func (c Config) validateExtractors() error {
	names := make([]string, 0, len(c.Extractors))
	for n := range c.Extractors {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := validateExtractor(n, c.Extractors[n]); err != nil {
			return err
		}
	}
	return nil
}

func validateExtractor(name string, e Extractor) error {
	at := "extractors." + name
	if !extractorNameRe.MatchString(name) {
		return fmt.Errorf("%s: the name must match [A-Za-z0-9._-]+", at)
	}
	if e.Type != ExtractorExec && e.Type != ExtractorLSP {
		return fmt.Errorf("%s.type: %q must be %q or %q", at, e.Type, ExtractorExec, ExtractorLSP)
	}
	if len(e.Argv) == 0 || e.Argv[0] == "" {
		return fmt.Errorf("%s.argv is required", at)
	}
	for _, a := range e.Argv {
		for _, tok := range placeholderRe.FindAllString(a, -1) {
			switch {
			case !knownPlaceholder(tok):
				return fmt.Errorf("%s.argv: unknown placeholder %s", at, tok)
			case tok == "{listFile}" && e.Type != ExtractorExec:
				return fmt.Errorf("%s.argv: {listFile} is only available to exec extractors", at)
			}
		}
	}
	for _, a := range e.VersionArgv {
		if toks := placeholderRe.FindAllString(a, -1); len(toks) > 0 {
			return fmt.Errorf("%s.version_argv: placeholders are not expanded (%s)", at, toks[0])
		}
	}
	for _, x := range e.Extensions {
		if !strings.HasPrefix(x, ".") {
			return fmt.Errorf("%s.extensions: %q must start with a dot", at, x)
		}
	}
	other := func(field string, set bool, want string) error {
		if set {
			return fmt.Errorf("%s.%s only applies to %s extractors", at, field, want)
		}
		return nil
	}
	if e.Type == ExtractorLSP {
		for _, f := range []struct {
			n string
			s bool
		}{{"timeout_s", e.TimeoutS != 0}, {"symbols", e.Symbols != nil}} {
			if err := other(f.n, f.s, ExtractorExec); err != nil {
				return err
			}
		}
	} else {
		for _, f := range []struct {
			n string
			s bool
		}{{"language_id", e.LanguageID != ""}, {"startup_timeout_s", e.StartupTimeoutS != 0}, {"request_timeout_s", e.RequestTimeoutS != 0}} {
			if err := other(f.n, f.s, ExtractorLSP); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateLanguageExtractors checks that a language names at least one
// extractor, that each exists, and that a configured extractor's extensions
// are a subset of the language's. (A built-in's extensions are a default
// filter: ExtractorsFor intersects them with the language's.)
func (c Config) validateLanguageExtractors(lang string, l Language) error {
	names := l.Extractor.Names()
	if len(names) == 0 {
		return fmt.Errorf("languages.%s.extractor is required (or the legacy command)", lang)
	}
	have := map[string]bool{}
	for _, x := range l.Extensions {
		have[strings.ToLower(x)] = true
	}
	for _, n := range names {
		e, builtin, ok := c.lookupExtractor(n)
		if !ok {
			return fmt.Errorf("languages.%s.extractor: unknown extractor %q (known: %s)", lang, n, strings.Join(c.extractorNames(), ", "))
		}
		if builtin {
			continue
		}
		for _, x := range e.Extensions {
			if !have[strings.ToLower(x)] {
				return fmt.Errorf("extractors.%s.extensions: %s is not an extension of language %s, which uses it", n, x, lang)
			}
		}
	}
	return nil
}

// extractorNames lists every extractor name that can be referenced, sorted.
func (c Config) extractorNames() []string {
	set := map[string]bool{}
	for n := range BuiltinExtractors() {
		set[n] = true
	}
	for n := range c.Extractors {
		set[n] = true
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
