// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/skip2/go-qrcode"
)

func pair(ctx context.Context, client *Client, stdout, stderr io.Writer) int {
	p, err := client.Pair(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	qr, err := renderQR(p.URI)
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprint(stdout, qr)
	fmt.Fprintf(stdout, "\nScan with the Notifier app, or enter by hand:\n  server  %s\n  code    %s  (expires %s, single use)\n",
		client.BaseURL, p.Code, p.ExpiresAt.Local().Format(time.Kitchen))
	return 0
}

// renderQR draws data as a QR code with half-block characters, two modules
// per character cell. Colours are explicit black on white, so the code reads
// the same (and scans) on dark and light terminals alike.
func renderQR(data string) (string, error) {
	q, err := qrcode.New(data, qrcode.Medium)
	if err != nil {
		return "", err
	}
	bitmap := q.Bitmap() // includes the 4-module quiet zone
	const (
		blackFG, whiteFG = "\x1b[38;5;16m", "\x1b[38;5;231m"
		blackBG, whiteBG = "\x1b[48;5;16m", "\x1b[48;5;231m"
		reset            = "\x1b[0m"
	)
	var b strings.Builder
	for y := 0; y < len(bitmap); y += 2 {
		for x := range bitmap[y] {
			top, bottom := bitmap[y][x], y+1 < len(bitmap) && bitmap[y+1][x]
			if top {
				b.WriteString(blackFG)
			} else {
				b.WriteString(whiteFG)
			}
			if bottom {
				b.WriteString(blackBG)
			} else {
				b.WriteString(whiteBG)
			}
			b.WriteString("▀")
		}
		b.WriteString(reset + "\n")
	}
	return b.String(), nil
}
