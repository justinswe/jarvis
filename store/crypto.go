package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"

	"github.com/justinswe/jarvis/worker/pkg/config"
	"github.com/justinswe/std/errors"
)

// sealSecret encrypts plaintext with AES-256-GCM under key, returning
// base64(nonce||ciphertext) for storage in a TEXT column. The external accounts API
// writes guild_mcp_servers rows with the same format and key.
//
// ponytail: single static key, no rotation, and no AAD binding a ciphertext to its
// (guild_id, name) row — an attacker who can already write the table could relocate one
// guild's token onto another guild's row. Both wants the same envelope change, so add a
// key-id prefix and pass the row identity as GCM additional data together.
func sealSecret(key []byte, plaintext string) (string, error) {
	aead, err := secretCipher(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", errors.Wrap(err, "generate secret nonce")
	}
	sealed := aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// openSecret decrypts a value produced by sealSecret.
func openSecret(key []byte, encoded string) (string, error) {
	aead, err := secretCipher(key)
	if err != nil {
		return "", err
	}
	sealed, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", errors.Wrap(err, "decode sealed secret")
	}
	if len(sealed) < aead.NonceSize() {
		return "", errors.New("sealed secret is truncated")
	}
	plaintext, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], nil)
	if err != nil {
		return "", errors.Wrap(err, "decrypt secret")
	}
	return string(plaintext), nil
}

func secretCipher(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, config.ErrMCPEncryptionUnavailable
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.Wrap(err, "create secret cipher")
	}
	return cipher.NewGCM(block)
}
