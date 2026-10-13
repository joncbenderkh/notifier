// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"
)

// The history keeps what this desktop received: at most maxHistoryEntries,
// none older than maxHistoryAge. The server keeps no content, so this is the
// only copy.
const (
	maxHistoryEntries = 500
	maxHistoryAge     = 30 * 24 * time.Hour
	// Appending is cheap; the file is pruned once it grows past this.
	compactAfterLines = 600
)

// historyEntry is one received message. Times are Unix seconds.
type historyEntry struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Body       string `json:"body,omitempty"`
	Priority   string `json:"priority"`
	SentAt     int64  `json:"sent_at"`
	ReceivedAt int64  `json:"received_at"`
	Channel    string `json:"channel,omitempty"`
	ChannelID  string `json:"channel_id,omitempty"`
}

// historyPath is $NOTIFYD_HISTORY, or notifyd-history.jsonl in
// $XDG_STATE_HOME/notifier (default ~/.local/state/notifier).
func historyPath() (string, error) {
	if p := os.Getenv("NOTIFYD_HISTORY"); p != "" {
		return p, nil
	}
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot locate the history file: %w", err)
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "notifier", "notifyd-history.jsonl"), nil
}

// history appends to a JSON-lines file, one message per line, readable only
// by its owner. It is not encrypted: it sits beside notifyd.json, whose
// seal key is in the clear under the same rule.
type history struct {
	path  string
	now   func() time.Time
	lines int // lines in the file, or -1 until counted
	// current reports whether the pairing that received a message is still
	// the saved one; checked under the lock, so a message received just
	// before a new pairing never lands in that pairing's history. nil: yes.
	current func() bool
}

func newHistory(path string, now func() time.Time) *history {
	return &history{path: path, now: now, lines: -1}
}

// record appends p. The file is opened for each message, and every change
// to it takes the history lock, so `notifyd history clear` or a new pairing
// in another process is never undone.
func (h *history) record(p Payload) error {
	return withHistoryLock(h.path, func() error {
		if h.current != nil && !h.current() {
			return nil
		}
		return h.append(p)
	})
}

func (h *history) append(p Payload) error {
	line, err := json.Marshal(historyEntry{
		ID: p.ID, Title: p.Title, Body: p.Body, Priority: p.Priority,
		SentAt: p.SentAt, ReceivedAt: h.now().Unix(), Channel: p.Channel, ChannelID: p.ChannelID,
	})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(h.path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(h.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	// The mode above applies only to a new file; never append to a readable one.
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	_, werr := f.Write(append(line, '\n'))
	if err := f.Close(); werr == nil {
		werr = err
	}
	if werr != nil {
		return werr
	}
	if h.lines < 0 {
		if h.lines, err = countLines(h.path); err != nil {
			return err
		}
	} else {
		h.lines++
	}
	if h.lines > compactAfterLines {
		return h.compact()
	}
	return nil
}

// compact rewrites the file with only what readHistory keeps.
func (h *history) compact() error {
	entries, err := readHistory(h.path, h.now())
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	for _, e := range slices.Backward(entries) { // oldest first, as appended
		line, err := json.Marshal(e)
		if err != nil {
			return err
		}
		buf.Write(append(line, '\n'))
	}
	if err := writeFileAtomic(h.path, buf.Bytes(), ".history-*.jsonl"); err != nil {
		return err
	}
	h.lines = len(entries)
	return nil
}

// readHistory returns the entries at path, newest first: each message once
// (as first received),
// none older than maxHistoryAge, at most maxHistoryEntries. Lines it cannot
// parse are skipped. A missing file is an empty history.
func readHistory(path string, now time.Time) ([]historyEntry, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var all []historyEntry
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		var e historyEntry
		if json.Unmarshal(scanner.Bytes(), &e) == nil && e.ID != "" {
			all = append(all, e)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	cutoff := now.Add(-maxHistoryAge).Unix()
	// The first copy of a message is when it arrived; later ones are
	// replays (shown again after the desktop failed to).
	seen := make(map[string]bool, len(all))
	var kept []historyEntry
	for _, e := range all {
		if seen[e.ID] {
			continue
		}
		seen[e.ID] = true
		if e.ReceivedAt > cutoff {
			kept = append(kept, e)
		}
	}
	// Later lines first, so same-second ties stay newest first after sorting.
	slices.Reverse(kept)
	slices.SortStableFunc(kept, func(a, b historyEntry) int { return cmp.Compare(b.ReceivedAt, a.ReceivedAt) })
	if len(kept) > maxHistoryEntries {
		kept = kept[:maxHistoryEntries]
	}
	return kept, nil
}

func countLines(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return bytes.Count(raw, []byte{'\n'}), nil
}

// clearHistory removes the history file; a missing one is already clear.
// Callers hold the history lock.
func clearHistory(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// withHistoryLock runs fn holding an exclusive lock shared by every notifyd
// process: the running daemon appends while `notifyd history clear`, pair,
// or unpair may run in another. The lock file stays; removing it would let
// two processes hold different locks.
func withHistoryLock(path string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}
