package extract

import (
	"fmt"

	"github.com/wamra/code-index/internal/config"
)

// newLSP builds an lsp extractor. It is a stub in this build; the
// implementation (the adapter from a language server to records) belongs to
// this file, which may delegate to a subpackage.
func newLSP(lang, name string, ec config.Extractor) (Extractor, error) {
	return nil, fmt.Errorf("extractor %q (type lsp) for language %q: not implemented in this build", name, lang)
}
