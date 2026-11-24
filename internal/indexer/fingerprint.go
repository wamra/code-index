package indexer

import (
	"context"

	"github.com/wamra/code-index/internal/config"
	"github.com/wamra/code-index/internal/extract"
)

// langFingerprint is the value stored in meta["edges_lang:<lang>"] once the
// language's edges are computed: extract.Fingerprint of the parts that
// extract.LangParts builds for its extractors in run order. It starts no
// language server; an extractor's version command runs once per call, so
// callers cache the result per run.
func langFingerprint(ctx context.Context, cfg config.Config, lang, root string) string {
	return extract.Fingerprint(extract.LangParts(ctx, lang, root, cfg.ExtractorsFor(lang)))
}
