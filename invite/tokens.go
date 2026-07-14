package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
)

// tokenBytes is the raw entropy of an invite token. 16 bytes is 128 bits, which
// is the credential for an unauthenticated redemption, so it must be
// unguessable rather than merely unique.
const tokenBytes = 16

// newToken returns a fresh URL-safe invite token and the sha256 hash stored in
// its place. The raw token is handed to the inviter exactly once (it appears in
// the link) and is never persisted; only the hash is written to the database,
// so a database leak does not hand out working links.
func newToken() (raw string, hash []byte) {
	b := make([]byte, tokenBytes)
	// crypto/rand.Read never returns an error in Go 1.24+; it panics internally
	// if the OS RNG fails, which is the correct behavior for key material.
	rand.Read(b)
	raw = base64.RawURLEncoding.EncodeToString(b)
	return raw, hashToken(raw)
}

// hashToken returns the sha256 of a raw token. Lookups hash the incoming token
// and match on the stored hash; the token is high-entropy, so a plain indexed
// equality lookup (not constant-time) is fine.
func hashToken(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}
