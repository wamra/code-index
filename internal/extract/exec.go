package extract

import (
	"fmt"

	"github.com/wamra/code-index/internal/config"
)

// newExec builds an exec extractor. It is a stub in this build; the
// implementation (a subprocess per distinct file list, JSON-lines output,
// grouping records by file for Emit) belongs to this file.
func newExec(lang, name string, ec config.Extractor) (Extractor, error) {
	return nil, fmt.Errorf("extractor %q (type exec) for language %q: not implemented in this build", name, lang)
}
