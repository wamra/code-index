package indexer

import (
	"context"

	"github.com/wamra/code-index/internal/config"
	"github.com/wamra/code-index/internal/extract"
)

// langFingerprint is the value stored in meta["edges_lang:<lang>"] once the
// language's edges are computed: extract.Fingerprint of its extractors in run
// order. It starts no language server; an extractor's version command runs
// once per call, so callers cache the result per run.
func langFingerprint(ctx context.Context, cfg config.Config, lang, root string) string {
	entries := cfg.ExtractorsFor(lang)
	parts := make([]extract.Part, 0, len(entries))
	for _, e := range entries {
		argv, err := extract.IdentityArgv(e.Argv, root)
		if err != nil {
			argv = e.Argv
		}
		parts = append(parts, extract.Part{
			Name:    e.Name,
			Type:    e.Type,
			Argv:    argv,
			Version: extract.ConfiguredVersion(ctx, e.Extractor),
		})
	}
	return extract.Fingerprint(parts)
}
