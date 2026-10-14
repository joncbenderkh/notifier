// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"

	"github.com/joncbenderkh/notifier/notifyd/internal/histfile"
)

// replyNonce, when set (by tests), replaces the random nonce.
var replyNonce []byte

// sealReply seals [answer] to the question [id] under its one-time key:
// AES-256-GCM, a fresh 12-byte nonce, "reply:" + the ID as associated data.
// Only the asker, who chose the key, can open it.
func sealReply(key, id, answer string) (nonce, ciphertext string, err error) {
	k, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		return "", "", fmt.Errorf("reply key: %w", err)
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return "", "", fmt.Errorf("reply key: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", "", err
	}
	plaintext, err := json.Marshal(map[string]string{"answer": answer})
	if err != nil {
		return "", "", err
	}
	n := replyNonce
	if n == nil {
		n = make([]byte, gcm.NonceSize())
		rand.Read(n)
	}
	ct := gcm.Seal(nil, n, plaintext, []byte("reply:"+id))
	return base64.StdEncoding.EncodeToString(n), base64.StdEncoding.EncodeToString(ct), nil
}

// answer sends [option] as this device's answer to the question [id] and
// returns what became of it, for the user, and whether it was taken.
func (a *app) answer(st State, id string, q histfile.Reply, option string) (string, bool) {
	if !slices.Contains(q.Options, option) {
		return fmt.Sprintf("%q is not one of the answers", printable(option, false)), false
	}
	if a.now().Unix() >= q.ExpiresAt {
		return "The question has expired", false
	}
	n, c, err := sealReply(q.Key, id, option)
	if err != nil {
		return "Could not seal the answer: " + err.Error(), false
	}
	err = a.call(http.MethodPost, st.URL+"/v1/replies/"+url.PathEscape(id), st.DeviceToken, map[string]string{"n": n, "c": c}, nil)
	var apiErr *apiError
	switch {
	case err == nil:
		return "Answered: " + printable(option, false), true
	case errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict:
		return "Already answered", false
	case errors.As(err, &apiErr) && apiErr.Status == http.StatusGone:
		return "The question has expired", false
	case errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound:
		return "This desktop may no longer answer", false
	}
	return "Could not send the answer: " + err.Error(), false
}

// reply answers the question [id] from the history with [option]; the
// fallback when the desktop's notifications show no buttons.
func (a *app) reply(id, option string) int {
	st, err := loadState(a.statePath)
	if err != nil {
		return a.fail(err)
	}
	entries, err := readHistory(a.historyPath, a.now())
	if err != nil {
		return a.fail(err)
	}
	i := slices.IndexFunc(entries, func(e historyEntry) bool { return e.ID == id })
	if i < 0 {
		return a.fail(fmt.Errorf("message %s is not in the history", id))
	}
	if entries[i].Reply == nil {
		return a.fail(fmt.Errorf("message %s asks nothing this desktop may answer", id))
	}
	outcome, ok := a.answer(st, id, *entries[i].Reply, option)
	if !ok {
		fmt.Fprintln(a.stderr, "notifyd:", outcome)
		return exitError
	}
	fmt.Fprintln(a.stdout, outcome)
	return 0
}
