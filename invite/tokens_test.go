package main

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewToken(t *testing.T) {
	t.Parallel()
	raw, hash := newToken()

	b, err := base64.RawURLEncoding.DecodeString(raw)
	require.NoError(t, err)
	assert.Len(t, b, tokenBytes, "token carries 128 bits of entropy")

	assert.Equal(t, hashToken(raw), hash, "returned hash matches hashToken of the raw token")
	assert.Len(t, hash, 32, "sha256 is 32 bytes")
}

func TestNewTokenUnique(t *testing.T) {
	t.Parallel()
	seen := make(map[string]struct{}, 2000)
	for range 2000 {
		raw, _ := newToken()
		_, dup := seen[raw]
		require.False(t, dup, "tokens must not collide")
		seen[raw] = struct{}{}
	}
}
