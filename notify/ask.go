// SPDX-License-Identifier: GPL-3.0-or-later

package main

// Questions (`notify ask`): the message carries the answer options and a
// one-time key. A device that may answer seals its choice under that key
// (AES-256-GCM, with "reply:" + the message ID as associated data), and the
// server keeps it, unreadable, until this command collects it.

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"time"
)

const (
	exitNoAnswer = 4

	replyKeyBytes = 32
	// maxPollSeconds is the longest the server holds a poll open.
	maxPollSeconds = 50
	minAskWait     = 10 * time.Second
	maxAskWait     = 24 * time.Hour
)

// retryPause is the first pause after a failed poll, and giveUpAfter how
// long to keep polling a question open for [wait]; tests shorten them.
var (
	retryPause  = time.Second
	giveUpAfter = func(wait time.Duration) time.Duration { return wait + time.Minute }
)

// Question is the part of a notification that asks for an answer.
type Question struct {
	Options   []string `json:"options"`
	Key       string   `json:"key"`
	ExpiresIn int      `json:"expires_in"`
	Anyone    bool     `json:"anyone,omitempty"`
}

// askCmd sends [args] (QUESTION [BODY | -]) with [options], then waits up
// to [wait] for the first answer and prints it.
func (c *cli) askCmd(args, to, channels, options []string, high *bool, wait time.Duration, anyone bool) int {
	if wait < minAskWait || wait > maxAskWait {
		fmt.Fprintf(c.stderr, "notify: --wait must be %s to %s\n", minAskWait, maxAskWait)
		return exitUsage
	}
	if len(options) == 0 {
		fmt.Fprintln(c.stderr, "notify: --options needs at least one answer")
		return exitUsage
	}
	key := make([]byte, replyKeyBytes)
	rand.Read(key)
	n, to := c.notification(args[0], to, channels, high)
	n.Question = &Question{
		Options:   options,
		Key:       base64.StdEncoding.EncodeToString(key),
		ExpiresIn: int(math.Ceil(wait.Seconds())),
		Anyone:    anyone,
	}
	// Standard output is kept for the answer. Other members' devices count
	// only when they may answer.
	client, id, code := c.deliver(n, to, args[1:], c.stderr, anyone)
	if id == "" {
		if code == exitUndelivered {
			fmt.Fprintln(c.stderr, "notify: no device that may answer got the question")
		}
		return code
	}
	return c.await(client, id, key, options, wait)
}

// await polls for the answer to [id] until the server reports one or the
// question expires.
func (c *cli) await(client *Client, id string, key []byte, options []string, wait time.Duration) int {
	// The server's clock decides; this one only stops a server that never does.
	giveUp := time.Now().Add(giveUpAfter(wait))
	var lastErr error
	for failures := 0; time.Now().Before(giveUp); {
		ctx, cancel := context.WithTimeout(context.Background(), maxPollSeconds*time.Second+c.timeout)
		r, err := client.Reply(ctx, id, maxPollSeconds)
		cancel()
		lastErr = err
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status < 500 && apiErr.Status != http.StatusTooManyRequests {
			return fail(c.stderr, err, c.sendCredName)
		}
		if err != nil {
			// A dropped connection or a server hiccup; the answer waits on
			// the server.
			if failures++; failures == 1 {
				fmt.Fprintln(c.stderr, "notify: still waiting:", err)
			}
			time.Sleep(min(time.Duration(failures)*retryPause, 30*time.Second))
			continue
		}
		failures = 0
		switch r.Status {
		case "answered":
			answer, err := openReply(key, id, r.N, r.C)
			if err != nil {
				fmt.Fprintln(c.stderr, "notify: unreadable answer:", err)
				return exitError
			}
			if !slices.Contains(options, answer) {
				fmt.Fprintf(c.stderr, "notify: the answer %q is not one of the options\n", clean(answer))
				return exitError
			}
			fmt.Fprintln(c.stderr, "answered by", clean(r.By))
			fmt.Fprintln(c.stdout, answer)
			return 0
		case "expired":
			fmt.Fprintf(c.stderr, "notify: no answer within %s\n", wait)
			return exitNoAnswer
		}
	}
	if lastErr != nil {
		// It may have been answered; only the server can say.
		fmt.Fprintln(c.stderr, "notify: could not get the answer:", lastErr)
		return exitError
	}
	fmt.Fprintf(c.stderr, "notify: no answer within %s\n", wait)
	return exitNoAnswer
}

// openReply opens a device's sealed answer to the question [id].
func openReply(key []byte, id, nonce, ciphertext string) (string, error) {
	n, err := base64.StdEncoding.DecodeString(nonce)
	if err != nil {
		return "", err
	}
	ct, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(n) != gcm.NonceSize() {
		return "", errors.New("bad nonce")
	}
	plaintext, err := gcm.Open(nil, n, ct, []byte("reply:"+id))
	if err != nil {
		return "", err
	}
	var r struct {
		Answer string `json:"answer"`
	}
	if err := json.Unmarshal(plaintext, &r); err != nil {
		return "", err
	}
	return r.Answer, nil
}
