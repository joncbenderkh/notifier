// SPDX-License-Identifier: GPL-3.0-or-later

package token

import (
	"regexp"
	"testing"
)

func TestPairingCodeShape(t *testing.T) {
	shape := regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{4}-[0-9A-HJKMNP-TV-Z]{4}$`)
	for range 100 {
		if code := NewPairingCode(); !shape.MatchString(code) {
			t.Fatalf("unexpected code %q", code)
		}
	}
}

func TestNormalizePairingCode(t *testing.T) {
	for input, want := range map[string]string{
		"7KQ2-MX9D":  "7KQ2-MX9D",
		"7kq2mx9d":   "7KQ2-MX9D",
		" 7kq2 mx9d": "7KQ2-MX9D",
		"OIL0-0000":  "0110-0000",
	} {
		if got := NormalizePairingCode(input); got != want {
			t.Errorf("NormalizePairingCode(%q) = %q, want %q", input, got, want)
		}
	}
}
