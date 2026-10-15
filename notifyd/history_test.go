// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joncbenderkh/notifier/notifyd/internal/histfile"
)

const testNow = 1_800_000_000

// TestMain keeps every test away from the user's real pairing and history,
// even one that forgets to set its own paths.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "notifyd-test")
	if err != nil {
		panic(err)
	}
	os.Setenv("NOTIFYD_STATE", filepath.Join(dir, "missing", "notifyd.json"))
	os.Setenv("NOTIFYD_HISTORY", filepath.Join(dir, "missing", "history.jsonl"))
	os.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func testHistory(t *testing.T) (*history, *int64) {
	now := int64(testNow)
	return newHistory(filepath.Join(t.TempDir(), "state", "h.jsonl"), func() time.Time { return time.Unix(now, 0) }), &now
}

func ids(entries []historyEntry) string {
	var out []string
	for _, e := range entries {
		out = append(out, e.ID)
	}
	return strings.Join(out, ",")
}

func TestHistoryKeepsTheNewestFirstAndEachMessageOnce(t *testing.T) {
	h, now := testHistory(t)
	for _, id := range []string{"a", "b", "a", "c"} {
		if err := h.record(Payload{ID: id, Title: id}); err != nil {
			t.Fatal(err)
		}
	}
	*now += 5
	h.record(Payload{ID: "d"})
	got, err := readHistory(h.path, time.Unix(*now, 0))
	// Same second: the order they first arrived in, newest first.
	if err != nil || ids(got) != "d,c,b,a" {
		t.Fatalf("got %q, %v", ids(got), err)
	}
	if info, _ := os.Stat(h.path); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode())
	}
	if info, _ := os.Stat(filepath.Dir(h.path)); info.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", info.Mode())
	}
}

func TestHistoryDropsOldEntriesAndCompacts(t *testing.T) {
	h, now := testHistory(t)
	h.record(Payload{ID: "old"})
	*now += int64(histfile.MaxAge / time.Second)
	for i := range compactAfterLines {
		h.record(Payload{ID: fmt.Sprintf("m%d", i)})
	}
	got, _ := readHistory(h.path, time.Unix(*now, 0))
	if len(got) != histfile.MaxEntries || strings.Contains(ids(got), "old") || got[0].ID != fmt.Sprintf("m%d", compactAfterLines-1) {
		t.Fatalf("%d entries, first %q", len(got), got[0].ID)
	}
	if lines, _ := countLines(h.path); lines > compactAfterLines {
		t.Fatalf("not compacted: %d lines", lines)
	}
	if info, _ := os.Stat(h.path); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode after compaction %v", info.Mode())
	}
}

func TestHistorySkipsDamagedLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.jsonl")
	os.WriteFile(path, []byte(`{"id":"a","received_at":1799999999}`+"\nnot json\n{\"title\":\"no id\"}\n"+`{"id":"b","received_at":1799999999}`), 0o600)
	got, err := readHistory(path, time.Unix(testNow, 0))
	if err != nil || ids(got) != "b,a" {
		t.Fatalf("got %q, %v", ids(got), err)
	}
	if got, err := readHistory(filepath.Join(t.TempDir(), "missing"), time.Unix(testNow, 0)); err != nil || got != nil {
		t.Fatalf("missing file: %v, %v", got, err)
	}
}

func TestRecordsBeforeShowingEvenWhatTheDesktopRefuses(t *testing.T) {
	h, _ := testHistory(t)
	srv := serveStream(t, func(c conn) {
		c.send(sealFrame(t, Payload{ID: "msg_poison", Title: "poison"}))
		c.ack()
		c.send(sealFrame(t, Payload{ID: "msg_ok", Title: "fine", Channel: "homelab"}))
		c.ack()
		c.send(sealFrame(t, Payload{ID: "msg_ok", Title: "fine"})) // a replay
		c.ack()
		c.ws.Close(closeUnpaired, "")
	})
	s := testStreamer(srv.URL, newFakeDesktop(), testToken)
	s.history = h
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.run(ctx); !errors.Is(err, errUnpaired) {
		t.Fatalf("run: %v", err)
	}
	if lines, _ := countLines(h.path); lines != 2 {
		t.Fatalf("%d lines recorded", lines)
	}
	got, _ := readHistory(h.path, time.Unix(testNow, 0))
	if ids(got) != "msg_ok,msg_poison" || got[0].Channel != "homelab" {
		t.Fatalf("got %+v", got)
	}
}

func TestHistoryCommand(t *testing.T) {
	a, stdout, stderr := newTestApp(t)
	if code := a.run([]string{"history"}); code != 0 || !strings.Contains(stdout.String(), "Nothing received yet.") {
		t.Fatalf("empty: exit %d, %s %s", code, stdout, stderr)
	}
	h := newHistory(a.historyPath, a.now)
	h.record(Payload{ID: "m1", Title: "Backup done", Priority: "default", SentAt: testNow})
	h.record(Payload{ID: "m2", Title: "Disk\x1b[2Jfull", Body: "line one\nline two", Priority: "high",
		SentAt: testNow - 3600, Channel: "alice/homelab", ChannelID: "ch_x"})
	h.record(Payload{ID: "m3", Title: "On default\u202e\u0085", Channel: "default", ChannelID: "ch_default_usr_1", SentAt: testNow,
		URL: "https://ci.example/7", Actions: []histfile.Action{{Label: "Lo\x1bgs", URL: "https://ci.example/7/logs"}}})

	stdout.Reset()
	if code := a.run([]string{"history", "-n", "2"}); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	out := stdout.String()
	for _, want := range []string{"On default", "[alice/homelab]  Disk�[2Jfull  (high)  (sent ", "    line one\n    line two\n",
		"    link: https://ci.example/7\n    Lo\ufffdgs: https://ci.example/7/logs\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Backup done") || strings.Contains(out, "[default]") ||
		strings.ContainsAny(out, "\x1b\u202e\u0085") || !strings.Contains(out, "On default\ufffd\ufffd") {
		t.Errorf("unexpected output:\n%s", out)
	}

	stdout.Reset()
	a.run([]string{"history", "--json", "-n", "0"})
	var entries []historyEntry
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil || ids(entries) != "m3,m2,m1" {
		t.Fatalf("json: %v, %q", err, ids(entries))
	}
	// Escaped, not raw, yet the same text once decoded.
	if strings.ContainsAny(stdout.String(), "\u202e\u0085") || entries[0].Title != "On default\u202e\u0085" {
		t.Fatalf("json output %q", stdout)
	}

	stdout.Reset()
	h.record(Payload{ID: "m2"}) // a replay: delete removes every copy
	if code := a.run([]string{"history", "delete", "m2", "m2"}); code != 0 || stdout.String() != "Deleted 1.\n" {
		t.Fatalf("delete: exit %d, %q %s", code, stdout, stderr)
	}
	if code := a.run([]string{"history", "delete", "m1", "nope"}); code != 1 || !strings.Contains(stderr.String(), "deleted 1 of 2") {
		t.Fatalf("delete missing: exit %d, %s", code, stderr)
	}
	if got, _ := readHistory(a.historyPath, a.now()); ids(got) != "m3" {
		t.Fatalf("after delete: %q", ids(got))
	}
	if info, _ := os.Stat(a.historyPath); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode after delete %v", info.Mode())
	}

	if code := a.run([]string{"history", "clear"}); code != 0 {
		t.Fatalf("clear: exit %d", code)
	}
	stdout.Reset()
	a.run([]string{"history", "--json"})
	if strings.TrimSpace(stdout.String()) != "[]" {
		t.Fatalf("after clear: %q", stdout)
	}
	for _, args := range [][]string{{"history", "extra"}, {"history", "-n", "-1"}, {"history", "clear", "now"}, {"history", "delete"}} {
		if code := a.run(args); code != exitUsage {
			t.Errorf("%v: exit %d", args, code)
		}
	}
}

func TestPairingAndUnpairingClearTheHistory(t *testing.T) {
	api := newFakeAPI(t)
	api.status["POST /v1/devices"] = http.StatusCreated
	api.answer["POST /v1/devices"] = registration
	api.status["DELETE /v1/device"] = http.StatusNoContent
	a, _, stderr := newTestApp(t)

	newHistory(a.historyPath, a.now).record(Payload{ID: "from_before"})
	if code := a.run([]string{"pair", "--server", api.URL, "CODE"}); code != 0 {
		t.Fatalf("pair: exit %d: %s", code, stderr)
	}
	if _, err := os.Stat(a.historyPath); !os.IsNotExist(err) {
		t.Fatal("pairing kept the previous history")
	}

	newHistory(a.historyPath, a.now).record(Payload{ID: "this_pairing"})
	if code := a.run([]string{"unpair"}); code != 0 {
		t.Fatalf("unpair: exit %d: %s", code, stderr)
	}
	if _, err := os.Stat(a.historyPath); !os.IsNotExist(err) {
		t.Fatal("unpairing kept the history")
	}
}

func TestNeverAppendsToAReadableFile(t *testing.T) {
	h, _ := testHistory(t)
	os.MkdirAll(filepath.Dir(h.path), 0o700)
	os.WriteFile(h.path, nil, 0o644)
	if err := h.record(Payload{ID: "secret"}); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(h.path); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode())
	}
}

func TestDropsMessagesOfAReplacedPairing(t *testing.T) {
	h, _ := testHistory(t)
	h.current = func() bool { return false } // paired anew since this daemon started
	if err := h.record(Payload{ID: "old_account"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.path); !os.IsNotExist(err) {
		t.Fatal("recorded a message of the previous pairing")
	}
}
