// SPDX-License-Identifier: GPL-3.0-or-later

package seal

import (
	"bytes"
	"encoding/base64"
	"testing"
)

// The same vector is asserted by the Android app's SealedMessageTest, pinning
// the wire format on both sides.
const (
	vectorKey        = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
	vectorNonce      = "MDEyMzQ1Njc4OWFi"
	vectorCiphertext = "TDM7F3Qdq2rkWAzPzRj+L2fQcGZjs92ayPLhYHDQE7A3+4ZuMxigPp5ZB5Zwv1bQKNFhPX3Zl57NQ/14A8U3Afl212jFtCR48XIXfkAPsHWEZmARTqUMCVVkb1gc7WGe8Sjshg42esye9bbN9nysV+9Pw6UNZw=="
	vectorDeviceID   = "dev_test"
	vectorPlaintext  = `{"id":"msg_test","title":"Hello","body":"From the test vector","priority":"high","sent_at":1791590400}`
)

func decode(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestOpenKnownVector(t *testing.T) {
	env := Envelope{Nonce: decode(t, vectorNonce), Ciphertext: decode(t, vectorCiphertext)}
	got, err := Open(decode(t, vectorKey), vectorDeviceID, env)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != vectorPlaintext {
		t.Fatalf("got %s", got)
	}
}

func TestRoundTrip(t *testing.T) {
	key, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	env, err := Seal(key, "dev_a", []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Open(key, "dev_a", env)
	if err != nil || !bytes.Equal(got, []byte("hello")) {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestOpenRejectsWrongDevice(t *testing.T) {
	key, _ := NewKey()
	env, _ := Seal(key, "dev_a", []byte("hello"))
	if _, err := Open(key, "dev_b", env); err == nil {
		t.Fatal("payload sealed for dev_a opened as dev_b")
	}
}

func TestOpenRejectsTampering(t *testing.T) {
	key, _ := NewKey()
	env, _ := Seal(key, "dev_a", []byte("hello"))
	env.Ciphertext[0] ^= 1
	if _, err := Open(key, "dev_a", env); err == nil {
		t.Fatal("tampered ciphertext opened")
	}
}
