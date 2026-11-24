package extract

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/wamra/code-index/internal/config"
)

// Part is the identity of one extractor in a language's fingerprint.
type Part struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	Argv    []string `json:"argv"`    // IdentityArgv of the extractor
	Version string   `json:"version"` // Extractor.Version
}

// PartOf builds the Part of e for the workspace at root. It starts no
// language server (Version is cheap by contract).
func PartOf(ctx context.Context, e Extractor, root string) (Part, error) {
	argv, err := IdentityArgv(e.Argv(), root)
	if err != nil {
		return Part{}, err
	}
	return Part{Name: e.Name(), Type: e.Type(), Argv: argv, Version: e.Version(ctx)}, nil
}

// LangParts builds the fingerprint parts of a language's extractors nes, in run
// order, through the built extractors (New, then PartOf). It starts no language
// server. An extractor that cannot be built or whose identity cannot be computed
// (validation rejects both, so this is a failsafe) gets a part that carries the
// error text as its version: the fingerprint then differs from every good one
// and from the one of another error, so stale data is never marked fresh.
func LangParts(ctx context.Context, lang, root string, nes []config.NamedExtractor) []Part {
	parts := make([]Part, 0, len(nes))
	for _, ne := range nes {
		ex, err := New(lang, ne.Name, ne.Extractor)
		var p Part
		if err == nil {
			p, err = PartOf(ctx, ex, root)
		}
		if err != nil {
			p = Part{Name: ne.Name, Type: ne.Type, Version: "fingerprint unavailable: " + err.Error()}
		}
		parts = append(parts, p)
	}
	return parts
}

// Fingerprint is the value stored in meta["edges_lang:<lang>"] once a
// language's edges are computed: the sha256 hex of SchemaVersion and the parts
// of the language's extractors, in run order. A language whose stored value
// differs from the current fingerprint is stale. The same inputs always give
// the same value.
func Fingerprint(parts []Part) string {
	type wire struct {
		Schema     int    `json:"schema"`
		Extractors []Part `json:"extractors"`
	}
	w := wire{Schema: SchemaVersion, Extractors: make([]Part, len(parts))}
	for i, p := range parts {
		if p.Argv == nil {
			p.Argv = []string{}
		}
		w.Extractors[i] = p
	}
	b, _ := json.Marshal(w)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
