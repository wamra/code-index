// Package embed turns pending chunks into vectors through a
// provider.Embedder (Ollama, OpenAI-compatible, Bedrock).
package embed

import (
	"context"
	"encoding/binary"
	"errors"
	"math"

	"github.com/wamra/code-index/internal/provider"
)

// ErrLocked means another process holds the embed lock.
var ErrLocked = errors.New("embed lock held by another process")

// Query embeds one search query with e's query prefix.
func Query(ctx context.Context, e provider.Embedder, q string) ([]float32, error) {
	return provider.Query(ctx, e, q)
}

// VecBytes encodes v as little-endian float32.
func VecBytes(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(f))
	}
	return b
}
