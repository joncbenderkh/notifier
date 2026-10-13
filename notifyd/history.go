// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/joncbenderkh/notifier/notifyd/internal/histfile"
)

// Appending is cheap; the file is pruned once it grows past this.
const compactAfterLines = 600

type historyEntry = histfile.Entry

var (
	historyPath     = histfile.Path
	readHistory     = histfile.Read
	clearHistory    = histfile.Clear
	withHistoryLock = histfile.WithLock
	writeFileAtomic = histfile.WriteFileAtomic
)

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
	if err := histfile.Write(h.path, entries); err != nil {
		return err
	}
	h.lines = len(entries)
	return nil
}

func countLines(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return bytes.Count(raw, []byte{'\n'}), nil
}
