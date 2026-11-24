package extract

import (
	"fmt"

	"github.com/wamra/code-index/internal/config"
)

// New builds the Extractor called name for the language lang from its resolved
// config entry ec (defaults filled in by config.Config.Resolved). It starts
// nothing. ec.Type selects the implementation:
//
//	"exec" -> newExec (exec.go)
//	"lsp"  -> newLSP  (lsp.go)
//
// An unknown type is an error (config validation rejects it earlier).
// This switch is the only place that knows both implementations; the
// implementations live in their own files so that they can be written
// independently.
func New(lang, name string, ec config.Extractor) (Extractor, error) {
	switch ec.Type {
	case config.ExtractorExec:
		return newExec(lang, name, ec)
	case config.ExtractorLSP:
		return newLSP(lang, name, ec)
	}
	return nil, fmt.Errorf("extractor %q: unknown type %q (want %q or %q)", name, ec.Type, config.ExtractorExec, config.ExtractorLSP)
}
