// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/joncbenderkh/notifier/notifyd/internal/histfile"
)

// Appending is cheap; the file is pruned once it grows past this, 100
// lines past what pruning keeps, reminders waiting included.
const compactAfterLines = histfile.MaxEntries + histfile.MaxReminders + 100

type historyEntry = histfile.Entry

var (
	historyPath     = histfile.Path
	readHistory     = histfile.Read
	clearHistory    = histfile.Clear
	withHistoryLock = histfile.WithLock
	writeFileAtomic = histfile.WriteFileAtomic
	markReminded    = histfile.Reminded
)

// history appends to a JSON-lines file, one message per line, readable only
// by its owner. It is not encrypted: it sits beside notifyd.json, whose
// seal key is in the clear under the same rule.
type history struct {
	path string
	now  func() time.Time
	// mu guards lines: the stream records while snoozing rewrites.
	mu    sync.Mutex
	lines int // lines in the file, or -1 until counted
	// current reports whether the pairing that received a message is still
	// the saved one; checked under the lock, so a message received just
	// before a new pairing never lands in that pairing's history. nil: yes.
	current func() bool
}

func newHistory(path string, now func() time.Time) *history {
	return &history{path: path, now: now, lines: -1}
}

// record appends p, and reports whether it waits for a reminder instead:
// the server delivered a snoozed message again, its ack lost, or a later
// message with its key is snoozed, whose reminder would replace it. The file is
// opened for each message, and every change to it takes the history lock,
// so `notifyd history clear` or a new pairing in another process is never
// undone.
func (h *history) record(p Payload) (snoozed bool, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	err = withHistoryLock(h.path, func() error {
		if h.current != nil && !h.current() {
			return nil
		}
		// Unreadable, it is recorded anyway: the message came. Its Seq,
		// unknown then, is taken from the clock, past any recorded.
		entries, readErr := readHistory(h.path, h.now())
		if slices.ContainsFunc(entries, func(e historyEntry) bool { return e.ID == p.ID && e.RemindAt != 0 }) {
			snoozed = true
			return nil
		}
		var seq int64
		if readErr != nil {
			seq = h.now().UnixNano()
		}
		for _, e := range entries {
			seq = max(seq, e.Seq)
		}
		e := historyEntry{
			ID: p.ID, Title: p.Title, Body: p.Body, Priority: p.Priority,
			SentAt: p.SentAt, ReceivedAt: h.now().Unix(), Channel: p.Channel, ChannelID: p.ChannelID,
			Reply: p.Reply, URL: p.URL, Actions: p.Actions, Key: p.Key, Seq: seq + 1,
		}
		// A replay is ordered as recorded, by its first arrival.
		if i := slices.IndexFunc(entries, func(r historyEntry) bool { return r.ID == p.ID }); i >= 0 {
			snoozed = histfile.BehindReminder(entries, i)
		} else {
			snoozed = histfile.BehindReminder(append([]historyEntry{e}, entries...), 0)
		}
		// Recorded even when that fails: the message came.
		dropErr := readErr
		if readErr == nil {
			var rewrote bool
			rewrote, dropErr = histfile.DropReplacedReminders(h.path, entries, e)
			if rewrote {
				h.lines = -1
			}
		}
		appendErr := h.append(e)
		if appendErr != nil {
			// Not recorded, so not behind the reminder either: shown.
			snoozed = false
		}
		return errors.Join(appendErr, dropErr)
	})
	return snoozed, err
}

func (h *history) append(e historyEntry) error {
	line, err := json.Marshal(e)
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

// errRepaired: the history belongs to another pairing now.
var errRepaired = errors.New("this desktop was paired again")

// remind sets when the message id is shown again, and reports whether the
// history still holds it.
func (h *history) remind(id string, at int64) (bool, error) {
	return h.rewrite(func() (bool, error) { return histfile.SetReminder(h.path, h.now(), id, at) })
}

// reminded records that message id's reminder for at showed, if it is
// still at, and reports whether it did.
func (h *history) reminded(id string, at int64) (bool, error) {
	return h.rewrite(func() (bool, error) { return markReminded(h.path, h.now(), id, at) })
}

func (h *history) rewrite(change func() (bool, error)) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var changed bool
	err := withHistoryLock(h.path, func() error {
		if h.current != nil && !h.current() {
			return errRepaired
		}
		var err error
		changed, err = change()
		h.lines = -1 // rewritten
		return err
	})
	return changed, err
}

// reminders returns the entries waiting to be shown again.
func (h *history) reminders() ([]historyEntry, error) {
	var due []historyEntry
	err := withHistoryLock(h.path, func() error {
		// An error, not none: the state file may be unreadable only for a moment.
		if h.current != nil && !h.current() {
			return errRepaired
		}
		entries, err := readHistory(h.path, h.now())
		// One a newer message replaced is dropped when that is recorded;
		// if that rewrite failed, it is still never shown over it.
		for i, e := range entries {
			if e.RemindAt != 0 && !histfile.Replaced(entries, i) {
				due = append(due, e)
			}
		}
		return err
	})
	return due, err
}

// whileWaiting calls show if message id still waits for its reminder at,
// under the history lock, so a deletion or a clear in another process
// comes either before, and nothing shows, or after. It reports whether
// the message still waited.
func (h *history) whileWaiting(id string, at int64, show func()) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var waiting bool
	err := withHistoryLock(h.path, func() error {
		// An error, not none: the state file may be unreadable only for a moment.
		if h.current != nil && !h.current() {
			return errRepaired
		}
		entries, err := readHistory(h.path, h.now())
		if err != nil {
			return err
		}
		for i, e := range entries {
			if e.ID == id && e.RemindAt == at && !histfile.Replaced(entries, i) {
				waiting = true
			}
		}
		if waiting {
			show()
		}
		return nil
	})
	return waiting, err
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
