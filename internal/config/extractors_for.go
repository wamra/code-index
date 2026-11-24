package config

import "strings"

// NamedExtractor is a resolved extractor entry together with its name.
type NamedExtractor struct {
	Name string
	Extractor
}

// ExtractorsFor returns the resolved extractors of a language in run order
// (nil for an unknown language; a name that matches no entry is skipped, Validate
// reports it). It works on an unresolved config too: it normalises first.
//
// The entries are ready for extract.New: defaults are filled, and
//   - Extensions: an entry that lists none gets all of the language's
//     extensions, and a built-in's list is intersected with the language's
//     (so it can be empty: the extractor then has no files);
//   - LanguageID of an lsp entry defaults to the language name;
//   - Symbols of an exec entry is set: when the config left it out it is true
//     only if the entry is the language's only extractor.
func (c Config) ExtractorsFor(lang string) []NamedExtractor {
	r := c.Resolved()
	l, ok := r.Languages[lang]
	if !ok {
		return nil
	}
	names := l.Extractor.Names()
	var out []NamedExtractor
	for _, n := range names {
		e, builtin, ok := r.lookupExtractor(n)
		if !ok {
			continue
		}
		e = e.withDefaults()
		e.Extensions = effectiveExtensions(e.Extensions, l.Extensions, builtin)
		switch e.Type {
		case ExtractorLSP:
			if e.LanguageID == "" {
				e.LanguageID = lang
			}
		case ExtractorExec:
			if e.Symbols == nil {
				only := len(names) == 1
				e.Symbols = &only
			}
		}
		out = append(out, NamedExtractor{Name: n, Extractor: e})
	}
	return out
}

func effectiveExtensions(own, lang []string, intersect bool) []string {
	if len(own) == 0 {
		return append([]string(nil), lang...)
	}
	if !intersect {
		return append([]string(nil), own...)
	}
	have := map[string]bool{}
	for _, x := range lang {
		have[strings.ToLower(x)] = true
	}
	var out []string
	for _, x := range own {
		if have[strings.ToLower(x)] {
			out = append(out, x)
		}
	}
	return out
}
