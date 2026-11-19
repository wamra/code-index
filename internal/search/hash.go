package search

import (
	"crypto/sha256"
	"encoding/hex"
)

func hashHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
