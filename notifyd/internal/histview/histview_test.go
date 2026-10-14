// SPDX-License-Identifier: GPL-3.0-or-later

package histview

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/joncbenderkh/notifier/notifyd/internal/histfile"
)

func TestWhen(t *testing.T) {
	now := time.Date(2026, 10, 14, 9, 30, 0, 0, time.Local)
	for _, tc := range []struct {
		t    time.Time
		want string
	}{
		{now.Add(-time.Hour), "08:30"},
		{now.Add(-10 * time.Hour), "Yesterday 23:30"},
		{now.AddDate(0, 0, -3), "Sun 09:30"},
		{now.AddDate(0, 0, -20), "24 Sep"},
		{now.AddDate(-1, 0, 0), "14 Oct 2025"},
	} {
		if got := When(tc.t, now); got != tc.want {
			t.Errorf("When(%v) = %q, want %q", tc.t, got, tc.want)
		}
	}
}

func TestWhenAcrossDaylightSaving(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip(err)
	}
	// 8 March 2026 has 23 hours there, 1 November 25.
	for _, tc := range []struct{ t, now time.Time }{
		{time.Date(2026, 3, 8, 0, 30, 0, 0, ny), time.Date(2026, 3, 9, 0, 10, 0, 0, ny)},
		{time.Date(2026, 11, 1, 23, 30, 0, 0, ny), time.Date(2026, 11, 2, 0, 10, 0, 0, ny)},
	} {
		if got := When(tc.t, tc.now); got[:9] != "Yesterday" {
			t.Errorf("When(%v, %v) = %q", tc.t, tc.now, got)
		}
	}
}

func TestTiming(t *testing.T) {
	sent := time.Date(2026, 10, 14, 7, 12, 0, 0, time.Local).Unix()
	e := histfile.Entry{SentAt: sent, ReceivedAt: sent + 30}
	if got := Timing(e); got != "Received Wed 14 Oct 2026, 07:12" {
		t.Errorf("prompt: %q", got)
	}
	e.ReceivedAt = sent + 2*3600 + 28*60
	if got := Timing(e); got != "Sent Wed 14 Oct 2026, 07:12 · received Wed 14 Oct 2026, 09:40 (delayed 2 h 28 min)" {
		t.Errorf("delayed: %q", got)
	}
}

func TestSearch(t *testing.T) {
	e := histfile.Entry{Title: "Disk FULL on nas", Body: "used\x1b[2J 98%\n<b>x</b>", Channel: "homelab", ChannelID: "ch_1"}
	for _, tc := range []struct {
		query, channel string
		want           bool
	}{
		{"", "", true}, {"full", "", true}, {"98%", "", true}, {"HOMELAB", "", true},
		{"full", "homelab", true}, {"full", "backups", false}, {"\x1b", "", false}, {"missing", "", false},
	} {
		if got := Matches(e, tc.query, tc.channel); got != tc.want {
			t.Errorf("Matches(%q, %q) = %v", tc.query, tc.channel, got)
		}
	}
	def := histfile.Entry{Title: "t", Channel: "default", ChannelID: "ch_default_usr_1"}
	if Channel(def) != "" || Matches(def, "", "default") {
		t.Error("the default channel must stay unnamed")
	}
	if got := Channels([]histfile.Entry{e, def, e, {Channel: "a/b", ChannelID: "ch_2"}}); len(got) != 2 || got[0] != "a/b" || got[1] != "homelab" {
		t.Errorf("Channels = %q", got)
	}
}

func TestHighlightEscapes(t *testing.T) {
	got := Highlight(`<i>Kelvin</i> & KELVIN`, "kelvin")
	want := `&lt;i&gt;<span background="#f6d32d" bgalpha="40%" weight="bold">Kelvin</span>&lt;/i&gt; &amp; <span background="#f6d32d" bgalpha="40%" weight="bold">KELVIN</span>`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	// U+212A KELVIN SIGN folds to k but is three bytes long.
	if got := Highlight("KB", "kb"); got != `<span background="#f6d32d" bgalpha="40%" weight="bold">`+"KB</span>" {
		t.Errorf("fold: %s", got)
	}
	if got := Highlight("a<b", ""); got != "a&lt;b" {
		t.Errorf("empty query: %s", got)
	}
}

func TestPreview(t *testing.T) {
	e := histfile.Entry{Body: "line one\n\n  line\ttwo‮"}
	if got := Preview(e, 100); got != "line one line two�" {
		t.Errorf("%q", got)
	}
	if got := Preview(e, 4); got != "line…" {
		t.Errorf("%q", got)
	}
}

func TestSeen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notifier", "bar-seen")
	if seen, err := ReadSeen(path); err != nil || len(seen) != 0 {
		t.Fatalf("missing: %v %v", seen, err)
	}
	entries := []histfile.Entry{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	if err := WriteSeen(path, map[string]bool{"a": true, "b": true, "gone": true}, entries); err != nil {
		t.Fatal(err)
	}
	if seen, err := ReadSeen(path); err != nil || !seen["a"] || !seen["b"] || len(seen) != 2 {
		t.Fatalf("%v %v", seen, err)
	}
	t.Setenv("SEEN", "/x/seen")
	if p, _ := SeenPath(); p != "/x/seen" {
		t.Error(p)
	}
	t.Setenv("SEEN", "")
	t.Setenv("XDG_STATE_HOME", "/state")
	if p, _ := SeenPath(); p != "/state/notifier/bar-seen" {
		t.Error(p)
	}
}
