// Package crypt encrypts file contents before they leave the device and
// decrypts them on the way back in. Every blob is encrypted at rest, always,
// with the symmetric key derived from the owner's Nostr key (see internal/keys).
//
// A sealed blob is self-describing: [12-byte nonce][AES-256-GCM ciphertext].
//
// Sealing is deterministic: the nonce is derived from the plaintext (keyed), so
// the same file under the same key always produces the same bytes and therefore
// the same Blossom content address. That is what stops two devices which copy in
// the same file from each uploading a distinct blob of it — with a random nonce
// they never converge, because the address is unpredictable until after sealing.
//
// The tradeoff is the standard one for deterministic encryption: someone with
// access to the Blossom server can see that two blobs hold identical plaintext.
// The nonce is keyed, so that equality never spans identities — only an owner's
// own blobs are comparable, and they already sit together behind one pubkey.
package crypt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
)

// nonceSize is the standard GCM nonce length in bytes.
const nonceSize = 12

// Overhead is how many bytes sealing adds: the nonce and the GCM tag. A sealed
// blob of plaintext n is exactly n+Overhead bytes, under either nonce scheme.
const Overhead = nonceSize + 16

// nonceDomain separates this derivation from any other use of the same key.
const nonceDomain = "tendrils/crypt/nonce/v1"

// Seal encrypts plaintext under key, returning nonce||ciphertext.
func Seal(key [32]byte, plaintext []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	// Seal appends the ciphertext to nonce, so the nonce prefixes the result.
	nonce := deriveNonce(key, plaintext)
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// deriveNonce computes the synthetic nonce for one plaintext — the SIV-style
// construction that makes sealing deterministic without reusing a nonce across
// different plaintexts (which under GCM would leak their XOR and, worse, the
// authentication key). Equal nonces imply equal plaintext up to an HMAC
// collision truncated to 96 bits, which at any realistic file count is far below
// the chance of a disk returning the wrong bytes.
//
// It is keyed rather than a bare hash of the plaintext, so the nonce reveals
// nothing to anyone without the owner's key — including whether two *different*
// owners hold the same file.
func deriveNonce(key [32]byte, plaintext []byte) []byte {
	mac := hmac.New(sha256.New, key[:])
	mac.Write([]byte(nonceDomain))
	mac.Write(plaintext)
	return mac.Sum(nil)[:nonceSize:nonceSize]
}

// Open reverses Seal. It returns an error if the blob is malformed or if the
// key is wrong (GCM authentication fails) — the same guard that makes a blob
// unreadable without the owner's key.
func Open(key [32]byte, sealed []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(sealed) < nonceSize {
		return nil, errors.New("crypt: sealed blob too short")
	}
	nonce, ciphertext := sealed[:nonceSize], sealed[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("crypt: open: %w", err)
	}
	return plaintext, nil
}

func newGCM(key [32]byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, fmt.Errorf("crypt: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypt: new gcm: %w", err)
	}
	return gcm, nil
}
