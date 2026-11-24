package extract

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
