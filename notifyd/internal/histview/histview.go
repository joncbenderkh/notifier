// SPDX-License-Identifier: GPL-3.0-or-later

// Package histview holds what the history window shows, apart from GTK:
// time labels, search, highlighting, and the i3blocks block's seen list.
package histview

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/joncbenderkh/notifier/notifyd/internal/histfile"
)

// Delayed is how much later than sent a message must arrive before the
// window shows both times.
const Delayed = time.Minute

// When labels a received time for the list: the time today, "Yesterday
// 15:04", the weekday within a week, else the date.
func When(t, now time.Time) string {
	// Calendar days, compared in UTC so a daylight saving change cannot
	// make one 23 or 25 hours long.
	day := func(t time.Time) time.Time { y, m, d := t.Date(); return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }
	days := int(day(now).Sub(day(t)).Hours() / 24)
	switch {
	case days <= 0:
		return t.Format("15:04")
	case days == 1:
		return "Yesterday " + t.Format("15:04")
	case days < 7:
		return t.Format("Mon 15:04")
	case t.Year() == now.Year():
		return t.Format("2 Jan")
	}
	return t.Format("2 Jan 2006")
}

// Timing describes when e was received, and when it was sent if it arrived
// noticeably later.
func Timing(e histfile.Entry) string {
	const layout = "Mon 2 Jan 2006, 15:04"
	received := time.Unix(e.ReceivedAt, 0)
	delay := time.Duration(e.ReceivedAt-e.SentAt) * time.Second
	if e.SentAt == 0 || delay < Delayed {
		return "Received " + received.Format(layout)
	}
	sent := time.Unix(e.SentAt, 0)
	return fmt.Sprintf("Sent %s · received %s (delayed %s)", sent.Format(layout), received.Format(layout), Duration(delay))
}

// Duration renders d coarsely: "2 h 28 min", "3 d 4 h", "45 s".
func Duration(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h %d min", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%d d %d h", int(d.Hours())/24, int(d.Hours())%24)
}

// Title and Body are e's text made safe to show: senders choose it, so
// control and format characters are replaced.
func Title(e histfile.Entry) string { return histfile.Printable(e.Title, false) }
func Body(e histfile.Entry) string  { return histfile.Printable(e.Body, true) }

// Channel is e's channel label, or "" for the default channel.
func Channel(e histfile.Entry) string {
	if e.OnDefaultChannel() {
		return ""
	}
	return histfile.Printable(e.Channel, false)
}

// Preview is the body on one line, cut to about maxRunes.
func Preview(e histfile.Entry, maxRunes int) string {
	line := strings.Join(strings.Fields(strings.ReplaceAll(Body(e), "\t", " ")), " ")
	if utf8.RuneCountInString(line) <= maxRunes {
		return line
	}
	return string([]rune(line)[:maxRunes]) + "…"
}

// Channels lists the channel labels in entries, sorted, once each.
func Channels(entries []histfile.Entry) []string {
	var out []string
	for _, e := range entries {
		if c := Channel(e); c != "" {
			out = append(out, c)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Matches reports whether e is on channel ("" for any) and holds query
// (ignoring case) in its title, body or channel.
func Matches(e histfile.Entry, query, channel string) bool {
	if channel != "" && Channel(e) != channel {
		return false
	}
	if query = strings.TrimSpace(query); query == "" {
		return true
	}
	return len(find(Title(e), query)) > 0 || len(find(Body(e), query)) > 0 || len(find(Channel(e), query)) > 0
}

// Highlight returns text as Pango markup with every match of query (ignoring
// case) emphasised. Everything else is escaped, so the result shows text
// literally.
func Highlight(text, query string) string {
	query = strings.TrimSpace(query)
	var b strings.Builder
	last := 0
	for _, m := range find(text, query) {
		b.WriteString(Escape(text[last:m[0]]))
		b.WriteString(`<span background="#f6d32d" bgalpha="40%" weight="bold">` + Escape(text[m[0]:m[1]]) + `</span>`)
		last = m[1]
	}
	b.WriteString(Escape(text[last:]))
	return b.String()
}

// find returns the byte ranges in text that equal query ignoring case,
// without overlaps. Matching compares runes, so case folding that changes a
// character's byte length still lines up.
func find(text, query string) [][2]int {
	n := utf8.RuneCountInString(query)
	if n == 0 {
		return nil
	}
	var out [][2]int
	for i := 0; i < len(text); {
		end, runes := i, 0
		for end < len(text) && runes < n {
			_, size := utf8.DecodeRuneInString(text[end:])
			end += size
			runes++
		}
		if runes < n {
			break
		}
		if strings.EqualFold(text[i:end], query) {
			out = append(out, [2]int{i, end})
			i = end
			continue
		}
		_, size := utf8.DecodeRuneInString(text[i:])
		i += size
	}
	return out
}

// Escape makes s safe inside Pango markup.
func Escape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;").Replace(s)
}

// SeenPath is where the i3blocks block reads which messages the window has
// listed: $SEEN, or bar-seen in $XDG_STATE_HOME/notifier, as the block
// expects.
func SeenPath() (string, error) {
	if p := os.Getenv("SEEN"); p != "" {
		return p, nil
	}
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "notifier", "bar-seen"), nil
}

// ReadSeen returns the IDs in the seen file; a missing file holds none.
func ReadSeen(path string) (map[string]bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		if id := strings.TrimSpace(scanner.Text()); id != "" {
			seen[id] = true
		}
	}
	return seen, scanner.Err()
}

// WriteSeen records the seen IDs that are still in entries, one per line,
// so the file does not outgrow the history.
func WriteSeen(path string, seen map[string]bool, entries []histfile.Entry) error {
	var b bytes.Buffer
	for _, e := range entries {
		if seen[e.ID] {
			b.WriteString(e.ID + "\n")
		}
	}
	return histfile.WriteFileAtomic(path, b.Bytes(), ".bar-seen-*")
}
