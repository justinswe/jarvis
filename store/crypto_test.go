package store

import (
	"bytes"
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSealSecretRoundTripsAndAuthenticates(t *testing.T) {
	key := testMCPKey()
	sealed, err := sealSecret(key, "the-token")
	require.NoError(t, err)
	opened, err := openSecret(key, sealed)
	require.NoError(t, err)
	assert.Equal(t, "the-token", opened)

	again, err := sealSecret(key, "the-token")
	require.NoError(t, err)
	assert.NotEqual(t, sealed, again, "every seal uses a fresh nonce")

	raw, err := base64.StdEncoding.DecodeString(sealed)
	require.NoError(t, err)
	raw[len(raw)-1] ^= 1
	_, err = openSecret(key, base64.StdEncoding.EncodeToString(raw))
	assert.Error(t, err, "a tampered ciphertext must not decrypt")

	_, err = openSecret(bytes.Repeat([]byte{9}, 32), sealed)
	assert.Error(t, err, "a different key must not decrypt")

	_, err = sealSecret(nil, "the-token")
	assert.ErrorContains(t, err, "not configured")
	_, err = openSecret(key, "@@not-base64@@")
	assert.Error(t, err)
	_, err = openSecret(key, base64.StdEncoding.EncodeToString([]byte("tiny")))
	assert.ErrorContains(t, err, "truncated")
}
