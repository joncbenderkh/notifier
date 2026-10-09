// SPDX-License-Identifier: GPL-3.0-or-later

// Package seal encrypts notification payloads end to end, so the push
// transport (FCM) can neither read nor forge them.
//
// The scheme is AES-256-GCM under a per-device key, with the device ID as
// associated data: a payload sealed for one device fails to open on another.
package seal

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

const KeySize = 32

// Envelope is the wire form carried in the FCM data message.
type Envelope struct {
	Nonce      []byte
	Ciphertext []byte
}

func NewKey() ([]byte, error) {
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return key, nil
}

func Seal(key []byte, deviceID string, plaintext []byte) (Envelope, error) {
	aead, err := newAEAD(key)
	if err != nil {
		return Envelope{}, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return Envelope{}, err
	}
	return Envelope{Nonce: nonce, Ciphertext: aead.Seal(nil, nonce, plaintext, []byte(deviceID))}, nil
}

func Open(key []byte, deviceID string, env Envelope) ([]byte, error) {
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	if len(env.Nonce) != aead.NonceSize() {
		return nil, errors.New("seal: invalid nonce size")
	}
	return aead.Open(nil, env.Nonce, env.Ciphertext, []byte(deviceID))
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("seal: key must be %d bytes, got %d", KeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
