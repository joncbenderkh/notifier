// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/joncbenderkh/notifier/notifyd/internal/histfile"
)

// envelopeVersion is the sealed format this daemon reads (server/src/seal.ts).
const envelopeVersion = "1"

// Payload is the plaintext of a message, as server/src/app.ts seals it.
type Payload struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Body      string `json:"body,omitempty"`
	Priority  string `json:"priority"`
	SentAt    int64  `json:"sent_at"`
	Channel   string `json:"channel,omitempty"`
	ChannelID string `json:"channel_id,omitempty"`
	// Reply is set when this device may answer the message's question.
	Reply *histfile.Reply `json:"reply,omitempty"`
}

// openEnvelope decrypts envelope v1: AES-256-GCM with a 12-byte nonce, the
// device ID as associated data, nonce and ciphertext in standard base64.
func openEnvelope(sealKey, deviceID, nonce, ciphertext string) (Payload, error) {
	var p Payload
	key, err := base64.StdEncoding.DecodeString(sealKey)
	if err != nil {
		return p, fmt.Errorf("seal key: %w", err)
	}
	n, err := base64.StdEncoding.DecodeString(nonce)
	if err != nil {
		return p, fmt.Errorf("nonce: %w", err)
	}
	c, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return p, fmt.Errorf("ciphertext: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return p, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return p, err
	}
	if len(n) != gcm.NonceSize() {
		return p, fmt.Errorf("nonce is %d bytes, want %d", len(n), gcm.NonceSize())
	}
	plaintext, err := gcm.Open(nil, n, c, []byte(deviceID))
	if err != nil {
		return p, fmt.Errorf("cannot decrypt: %w", err)
	}
	if err := json.Unmarshal(plaintext, &p); err != nil {
		return p, fmt.Errorf("payload: %w", err)
	}
	return p, nil
}
