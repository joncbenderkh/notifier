// SPDX-License-Identifier: GPL-3.0-or-later

// Package histfile reads and changes the history file that notifyd keeps of
// what this desktop received, for the daemon and the history window alike.
package histfile

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode"
)

// The history keeps at most MaxEntries, none older than MaxAge. The server
// keeps no content, so this is the only copy.
const (
	MaxEntries = 500
	MaxAge     = 30 * 24 * time.Hour
)

// Entry is one received message. Times are Unix seconds.
type Entry struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Body       string `json:"body,omitempty"`
	Priority   string `json:"priority"`
	SentAt     int64  `json:"sent_at"`
	ReceivedAt int64  `json:"received_at"`
	Channel    string `json:"channel,omitempty"`
	ChannelID  string `json:"channel_id,omitempty"`
	Reply      *Reply `json:"reply,omitempty"`
	// URL is the link a tap on the message opens; Actions its buttons.
	URL     string   `json:"url,omitempty"`
	Actions []Action `json:"actions,omitempty"`
	// RemindAt is when a snoozed message is shown again; 0 for none.
	RemindAt int64 `json:"remind_at,omitempty"`
	// Key is its key: a later message with the same key, in the same
	// channel, replaces it.
	Key string `json:"key,omitempty"`
	// Seq is its place in the order of arrival, 1 for the first; 0 in
	// histories from before it was kept. ReceivedAt follows the clock,
	// which can be set back.
	Seq int64 `json:"seq,omitempty"`
	// Reminded: it was snoozed and shown again, ReceivedAt then, so it
	// was not delayed on the way.
	Reminded bool `json:"reminded,omitempty"`
}

// Action is a button that opens its link.
type Action struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

// WebLink reports whether link is one a message may open: http or https,
// with a host and no credentials. The server accepts no other, but other
// servers and old history may hold one. host is where it leads, to show.
func WebLink(link string) (host string, ok bool) {
	u, err := url.Parse(link)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return "", false
	}
	return u.Hostname(), true
}

// Reply is a question's answers, the one-time key to seal one under, and
// when the question closes (Unix seconds); only devices that may answer
// get it.
type Reply struct {
	Options   []string `json:"options"`
	Key       string   `json:"key"`
	ExpiresAt int64    `json:"expires_at"`
}

// OnDefaultChannel reports whether e went to the user's default channel,
// which clients leave unnamed.
func (e Entry) OnDefaultChannel() bool {
	return e.Channel == "" || strings.HasPrefix(e.ChannelID, "ch_default_")
}

// Path is $NOTIFYD_HISTORY, or notifyd-history.jsonl in
// $XDG_STATE_HOME/notifier (default ~/.local/state/notifier).
func Path() (string, error) {
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

// Read returns the entries at path, newest first: each message once (as
// first received), none older than MaxAge, at most MaxEntries. Lines it
// cannot parse are skipped. A missing file is an empty history.
func Read(path string, now time.Time) ([]Entry, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var all []Entry
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		var e Entry
		if json.Unmarshal(scanner.Bytes(), &e) == nil && e.ID != "" {
			all = append(all, e)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	// The first copy of a message is when it arrived; later ones are
	// replays (shown again after the desktop failed to).
	seen := make(map[string]bool, len(all))
	var kept []Entry
	for _, e := range all {
		if !seen[e.ID] {
			seen[e.ID] = true
			kept = append(kept, e)
		}
	}
	// Later lines first, so same-second ties stay newest first after sorting.
	slices.Reverse(kept)
	slices.SortStableFunc(kept, func(a, b Entry) int { return cmp.Compare(b.ReceivedAt, a.ReceivedAt) })
	// A snoozed message stays until its reminder shows, past MaxAge and
	// MaxEntries; not one a newer message replaced, whose reminder never will.
	waiting := make([]bool, len(kept))
	for i, e := range kept {
		waiting[i] = e.RemindAt != 0 && !Replaced(kept, i)
	}
	cutoff := now.Add(-MaxAge).Unix()
	others := 0
	var out []Entry
	for i, e := range kept {
		if !waiting[i] {
			// Its reminder never shows, even once what replaced it is pruned.
			e.RemindAt = 0
			if e.ReceivedAt <= cutoff {
				continue
			}
			if others++; others > MaxEntries {
				continue
			}
		}
		out = append(out, e)
	}
	return out, nil
}

// Write replaces the file at path with entries, given newest first as Read
// returns them. Callers hold the lock.
func Write(path string, entries []Entry) error {
	var buf bytes.Buffer
	for _, e := range slices.Backward(entries) { // oldest first, as appended
		line, err := json.Marshal(e)
		if err != nil {
			return err
		}
		buf.Write(append(line, '\n'))
	}
	return WriteFileAtomic(path, buf.Bytes(), ".history-*.jsonl")
}

// Delete removes the entries with the given IDs and reports how many it
// found. Like a compaction, it also drops what Read would not return.
// Callers hold the lock.
func Delete(path string, now time.Time, ids ...string) (int, error) {
	entries, err := Read(path, now)
	if err != nil {
		return 0, err
	}
	kept := slices.DeleteFunc(slices.Clone(entries), func(e Entry) bool { return slices.Contains(ids, e.ID) })
	removed := len(entries) - len(kept)
	if removed == 0 {
		return 0, nil
	}
	return removed, Write(path, kept)
}

// SetReminder sets when the entry with ID id is shown again (0: never)
// and reports whether it did: not when the history no longer holds it, or
// a later message replaced it. Like a compaction, it also drops what Read
// would not return. Callers hold the lock.
func SetReminder(path string, now time.Time, id string, at int64) (bool, error) {
	return setReminder(path, now, id, at, func(entries []Entry, i int) bool { return !Replaced(entries, i) })
}

// Replaced reports whether a later message with the key of entries[i], in
// its channel, took its place: sent later, or in the same second and
// arrived later, by Seq, or failing that (from before it was kept), by
// the order of entries, newest first as Read returns them. By sending
// time first, so a replay pruned from the history replaces nothing.
func Replaced(entries []Entry, i int) bool {
	e := entries[i]
	if e.Key == "" {
		return false
	}
	for j, n := range entries {
		if j == i || n.Key != e.Key || n.ChannelID != e.ChannelID {
			continue
		}
		if n.SentAt != e.SentAt {
			if n.SentAt > e.SentAt {
				return true
			}
			continue
		}
		if n.Seq != e.Seq && n.Seq > e.Seq || n.Seq == e.Seq && j < i {
			return true
		}
	}
	return false
}

// DropReplacedReminders clears the reminders that e, about to be recorded,
// replaces among entries, as Read returned them from path, and reports
// whether it rewrote the file: when it arrives, not when they come, as
// the entry limit may remove e first. A replay of a recorded message
// replaces nothing, nor does one sent before the reminder's. Callers hold
// the lock.
func DropReplacedReminders(path string, entries []Entry, e Entry) (bool, error) {
	if e.Key == "" {
		return false, nil
	}
	entries = slices.Clone(entries)
	if slices.ContainsFunc(entries, func(o Entry) bool { return o.ID == e.ID }) {
		return false, nil
	}
	changed := false
	for i, o := range entries {
		if o.RemindAt != 0 && o.Key == e.Key && o.ChannelID == e.ChannelID && o.SentAt <= e.SentAt {
			entries[i].RemindAt = 0
			changed = true
		}
	}
	if !changed {
		return false, nil
	}
	return true, Write(path, entries)
}

// Reminded records that the reminder of the entry with ID id showed at
// now, if it is still at, and reports whether it did: one set again
// meanwhile stays. The entry counts as received then, so it stays its
// MaxAge, and a snooze from the reminder finds it.
// Callers hold the lock.
func Reminded(path string, now time.Time, id string, at int64) (bool, error) {
	return setReminder(path, now, id, 0, func(entries []Entry, i int) bool {
		if entries[i].RemindAt != at {
			return false
		}
		entries[i].ReceivedAt = now.Unix()
		entries[i].Reminded = true
		return true
	})
}

func setReminder(path string, now time.Time, id string, at int64, ok func([]Entry, int) bool) (bool, error) {
	entries, err := Read(path, now)
	if err != nil {
		return false, err
	}
	i := slices.IndexFunc(entries, func(e Entry) bool { return e.ID == id })
	if i < 0 || !ok(entries, i) {
		return false, nil
	}
	entries[i].RemindAt = at
	return true, Write(path, entries)
}

// Clear removes the history file; a missing one is already clear. Callers
// hold the lock.
func Clear(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// WithLock runs fn holding an exclusive lock shared by every process that
// changes the history: the running daemon appends while `notifyd history
// clear`, pair, unpair, or the history window may run in another. The lock
// file stays; removing it would let two processes hold different locks.
func WithLock(path string, fn func() error) error {
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

// WriteFileAtomic replaces path with data, readable only by its owner: a
// temporary file (named by pattern) is synced, then renamed over it. A
// symlink is written through rather than replaced.
func WriteFileAtomic(path string, data []byte, pattern string) error {
	if target, err := filepath.EvalSymlinks(path); err == nil {
		path = target
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, pattern) // created 0600
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		d.Sync() // best effort
		d.Close()
	}
	return nil
}

// Printable replaces control and format characters (terminal escapes, bidi
// overrides, zero-width characters) with U+FFFD, keeping tabs, and newlines
// only when multiline. Senders choose the text.
func Printable(s string, multiline bool) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' || multiline && r == '\n' || !Invisible(r) {
			return r
		}
		return '�'
	}, s)
}

// Invisible reports whether r is a control or format character.
func Invisible(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }
