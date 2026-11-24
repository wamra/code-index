package config

// NamedExtractor is a resolved extractor entry with its config name.
type NamedExtractor struct {
	Name string
	Extractor
}

// ExtractorsFor returns the extractor entries of language lang in run order:
// the implicit language server of the legacy Command field first, then the
// extractors named by the language's "extractor", each taken from
// Config.Extractors or else from BuiltinExtractors. Unknown names are skipped.
// It returns nil for a language that is not configured.
func (c Config) ExtractorsFor(lang string) []NamedExtractor {
	l, ok := c.Languages[lang]
	if !ok {
		return nil
	}
	var out []NamedExtractor
	if l.Command != "" {
		out = append(out, NamedExtractor{Name: lang + "-lsp", Extractor: Extractor{
			Type:            ExtractorLSP,
			Argv:            append([]string{l.Command}, l.Args...),
			Env:             l.Env,
			LanguageID:      l.LanguageID,
			StartupTimeoutS: l.StartupTimeoutS,
			RequestTimeoutS: l.RequestTimeoutS,
		}})
	}
	builtin := BuiltinExtractors()
	for _, name := range l.Extractor.Names() {
		e, ok := c.Extractors[name]
		if !ok {
			e, ok = builtin[name]
		}
		if ok {
			out = append(out, NamedExtractor{Name: name, Extractor: e})
		}
	}
	return out
}
