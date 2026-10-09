// SPDX-License-Identifier: GPL-3.0-or-later

// Package token generates and hashes the bearer secrets the server issues.
//
// Every secret carries 256 bits of entropy, so an unsalted SHA-256 digest is
// a sufficient at-rest representation: it cannot be brute-forced, and lookups
// by digest need no constant-time comparison.
package token

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"strings"
)

const (
	APIKeyPrefix      = "nk_"
	DeviceTokenPrefix = "nd_"
)

var encoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// pairingAlphabet is Crockford base32: no I, L, O, or U, so codes survive
// being read aloud or retyped.
const pairingAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

func New(prefix string) string {
	return prefix + strings.ToLower(encoding.EncodeToString(randomBytes(32)))
}

func NewID(prefix string) string {
	return prefix + strings.ToLower(encoding.EncodeToString(randomBytes(10)))
}

// NewPairingCode returns a short, human-typeable code like "7KQ2-MX9D".
func NewPairingCode() string {
	raw := randomBytes(8)
	var b strings.Builder
	for i, v := range raw {
		if i == 4 {
			b.WriteByte('-')
		}
		b.WriteByte(pairingAlphabet[v&31])
	}
	return b.String()
}

// NormalizePairingCode maps user input onto the canonical code form, so
// lowercase, missing dashes, and Crockford's look-alike letters still match.
func NormalizePairingCode(input string) string {
	replacer := strings.NewReplacer("-", "", " ", "", "I", "1", "L", "1", "O", "0")
	code := replacer.Replace(strings.ToUpper(input))
	if len(code) != 8 {
		return code
	}
	return code[:4] + "-" + code[4:]
}

func Hash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("token: crypto/rand failed: " + err.Error())
	}
	return b
}
