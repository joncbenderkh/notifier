// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

func TestOpensTheSharedVector(t *testing.T) {
	// The vector of server/test/seal.test.ts and Android's SealedMessageTest.
	p, err := openEnvelope(testKey, "dev_test", "MDEyMzQ1Njc4OWFi",
		"TDM7F3Qdq2rkWAzPzRj+L2fQcGZjs92ayPLhYHDQE7A3+4ZuMxigPp5ZB5Zwv1bQKNFhPX3Zl57NQ/14A8U3Afl212jFtCR48XIXfkAPsHWEZmARTqUMCVVkb1gc7WGe8Sjshg42esye9bbN9nysV+9Pw6UNZw==")
	if err != nil {
		t.Fatal(err)
	}
	want := Payload{ID: "msg_test", Title: "Hello", Body: "From the test vector", Priority: "high", SentAt: 1791590400}
	if p != want {
		t.Fatalf("got %+v", p)
	}
	if _, err := openEnvelope(testKey, "dev_other", "MDEyMzQ1Njc4OWFi", "TDM7F3Qdq2rkWAzPzRj+L2fQcGZjs92a"); err == nil {
		t.Fatal("opened under the wrong device ID")
	}
}

func TestParsesPairLinks(t *testing.T) {
	good := map[string][2]string{
		"notifier://pair?server=https%3A%2F%2Fnotifier.example&code=7KQ2-MX9D": {"https://notifier.example", "7KQ2-MX9D"},
		" notifier://pair?server=HTTPS://Notifier.Example:443/&code=7KQ2MX9D ": {"https://notifier.example", "7KQ2MX9D"},
		"notifier://pair?server=http://127.0.0.1:8787&code=X":                  {"http://127.0.0.1:8787", "X"},
		"notifier://pair?server=http://[::1]:8787&code=X":                      {"http://[::1]:8787", "X"},
	}
	for link, want := range good {
		server, code, err := parsePairLink(link)
		if err != nil || server != want[0] || code != want[1] {
			t.Errorf("parsePairLink(%q) = %q, %q, %v", link, server, code, err)
		}
	}
	for _, link := range []string{
		"notifier://join?server=https://notifier.example&code=X",
		"https://notifier.example/pair?code=X",
		"notifier://pair?server=https://notifier.example",
		"notifier://pair?server=http://notifier.example&code=X",
		"notifier://pair?server=http://192.168.1.2&code=X",
		"notifier://pair?server=https://trusted@evil.example&code=X",
		"notifier://pair?server=https://notifier.example/path&code=X",
		"notifier://pair?server=https://notifier.example?x=1&code=X",
		"notifier://pair?server=ftp://notifier.example&code=X",
	} {
		if _, _, err := parsePairLink(link); err == nil {
			t.Errorf("parsePairLink(%q) accepted", link)
		}
	}
}

func TestMapsPayloadsOntoNotify(t *testing.T) {
	summary, body, hints := notification(Payload{Title: "Disk", Body: "a < b & c", Priority: "high", Channel: "alice/homelab"}, true)
	if summary != "Disk" || body != "a &lt; b &amp; c" {
		t.Fatalf("summary %q, body %q", summary, body)
	}
	if hints["urgency"] != dbus.MakeVariant(urgencyCritical) || hints["category"] != dbus.MakeVariant("notifier.alice/homelab") {
		t.Fatalf("hints %v", hints)
	}
	_, body, hints = notification(Payload{Title: "Hi", Body: "a < b", Priority: "default"}, false)
	if body != "a < b" || hints["urgency"] != dbus.MakeVariant(urgencyNormal) {
		t.Fatalf("body %q, hints %v", body, hints)
	}
	if _, ok := hints["category"]; ok {
		t.Fatal("category without a channel")
	}
}

func TestDropsNULAndTellsRejectionsFromOutages(t *testing.T) {
	summary, body, hints := notification(Payload{Title: "a\x00b", Body: "c\x00d", Channel: "e\x00f"}, false)
	if summary != "ab" || body != "cd" || hints["category"] != dbus.MakeVariant("notifier.ef") {
		t.Fatalf("%q %q %v", summary, body, hints)
	}
	for _, c := range []struct {
		err  error
		want bool
	}{
		{dbus.FormatError("input has a null char"), true},
		{dbus.Error{Name: "org.freedesktop.DBus.Error.InvalidArgs"}, true},
		{dbus.Error{Name: "org.freedesktop.DBus.Error.ServiceUnknown"}, false},
		{dbus.Error{Name: "org.freedesktop.DBus.Error.NoReply"}, false},
		{context.DeadlineExceeded, false},
		{errors.New("dbus: connection closed by user"), false},
	} {
		if got := permanent(c.err); got != c.want {
			t.Errorf("permanent(%v) = %v", c.err, got)
		}
	}
}

func TestStateRoundTripAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notifier", "notifyd.json")
	if _, err := loadState(path); !errors.Is(err, errNotPaired) {
		t.Fatalf("missing file: %v", err)
	}
	st := State{URL: "https://n.example", DeviceID: "dev_1", DeviceToken: "nd_1", SealKey: testKey, Name: "desk"}
	if err := saveState(path, st); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode())
	}
	if got, err := loadState(path); err != nil || got != st {
		t.Fatalf("got %+v, %v", got, err)
	}
	os.Chmod(path, 0o644)
	if _, err := loadState(path); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("world-readable: %v", err)
	}
	os.WriteFile(path, []byte(`{"url":"x","extra":1}`), 0o600)
	if _, err := loadState(path); err == nil {
		t.Fatal("accepted an unknown field")
	}
}

// fakeAPI answers the device routes and records what it was sent.
type fakeAPI struct {
	*httptest.Server
	requests []string
	bodies   []map[string]string
	status   map[string]int
	answer   map[string]string
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{status: map[string]int{}, answer: map[string]string{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		f.requests = append(f.requests, key+" "+r.Header.Get("Authorization"))
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		f.bodies = append(f.bodies, body)
		status, ok := f.status[key]
		if !ok {
			status = http.StatusNotFound
		}
		w.WriteHeader(status)
		w.Write([]byte(f.answer[key]))
	}))
	t.Cleanup(f.Close)
	return f
}

func newTestApp(t *testing.T) (*app, *bytes.Buffer, *bytes.Buffer) {
	var stdout, stderr bytes.Buffer
	dir := t.TempDir()
	return &app{
		stdout:      &stdout,
		stderr:      &stderr,
		statePath:   filepath.Join(dir, "notifyd.json"),
		historyPath: filepath.Join(dir, "state", "notifyd-history.jsonl"),
		now:         func() time.Time { return time.Unix(testNow, 0) },
		http:        &http.Client{CheckRedirect: refuseRedirect},
		desktop:     func(context.Context) (Desktop, error) { return newFakeDesktop(), nil },
		hostname:    func() (string, error) { return "workstation", nil },
		signals:     func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) },
	}, &stdout, &stderr
}

const registration = `{"device_id":"dev_new","device_token":"nd_new","seal_key":"` + testKey + `"}`

func TestPairsWithALinkAndAsWebSocketDevice(t *testing.T) {
	api := newFakeAPI(t)
	api.status["POST /v1/devices"] = http.StatusCreated
	api.answer["POST /v1/devices"] = registration
	a, stdout, stderr := newTestApp(t)

	link := "notifier://pair?server=" + api.URL + "&code=7KQ2-MX9D"
	if code := a.run([]string{"pair", link}); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if got := api.bodies[0]; got["code"] != "7KQ2-MX9D" || got["name"] != "workstation" || got["transport"] != "ws" || len(got) != 3 {
		t.Fatalf("sent %v", got)
	}
	st, err := loadState(a.statePath)
	if err != nil || st.DeviceToken != "nd_new" || st.URL != api.URL || st.Name != "workstation" {
		t.Fatalf("state %+v, %v", st, err)
	}
	if !strings.Contains(stdout.String(), "dev_new") {
		t.Fatalf("stdout %q", stdout)
	}

	if code := a.run([]string{"pair", link}); code != exitError || !strings.Contains(stderr.String(), "already paired") {
		t.Fatalf("second pair: exit %d, %s", code, stderr)
	}
}

func TestForcedPairUnpairsTheOldDevice(t *testing.T) {
	api := newFakeAPI(t)
	api.status["POST /v1/devices"] = http.StatusCreated
	api.answer["POST /v1/devices"] = registration
	api.status["DELETE /v1/device"] = http.StatusNoContent
	a, _, stderr := newTestApp(t)
	saveState(a.statePath, State{URL: api.URL, DeviceID: "dev_old", DeviceToken: "nd_old", SealKey: testKey, Name: "old"})

	if code := a.run([]string{"pair", "--force", "--server", api.URL, "--name", "desk", "CODE"}); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if len(api.requests) != 2 || api.bodies[0]["name"] != "desk" || api.requests[1] != "DELETE /v1/device Bearer nd_old" {
		t.Fatalf("requests %v, bodies %v", api.requests, api.bodies)
	}
	if st, _ := loadState(a.statePath); st.DeviceToken != "nd_new" {
		t.Fatalf("state %+v", st)
	}
}

func TestForcedPairWithABadCodeKeepsTheOldPairing(t *testing.T) {
	api := newFakeAPI(t)
	api.status["POST /v1/devices"] = http.StatusForbidden
	api.answer["POST /v1/devices"] = `{"error":"pairing code is invalid, expired, or already used"}`
	a, _, _ := newTestApp(t)
	old := State{URL: api.URL, DeviceID: "dev_old", DeviceToken: "nd_old", SealKey: testKey, Name: "old"}
	saveState(a.statePath, old)
	if code := a.run([]string{"pair", "--force", "--server", api.URL, "BAD"}); code != exitError {
		t.Fatalf("exit %d", code)
	}
	if len(api.requests) != 1 || !strings.HasPrefix(api.requests[0], "POST") {
		t.Fatalf("requests %v", api.requests)
	}
	if st, _ := loadState(a.statePath); st != old {
		t.Fatalf("state %+v", st)
	}
}

func TestPairReportsTheServersRefusal(t *testing.T) {
	api := newFakeAPI(t)
	api.status["POST /v1/devices"] = http.StatusForbidden
	api.answer["POST /v1/devices"] = `{"error":"pairing code is invalid, expired, or already used"}`
	a, _, stderr := newTestApp(t)
	if code := a.run([]string{"pair", "--server", api.URL, "CODE"}); code != exitError || !strings.Contains(stderr.String(), "already used") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if _, err := os.Stat(a.statePath); err == nil {
		t.Fatal("saved a failed pairing")
	}
}

func TestPairUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"pair"},
		{"pair", "CODE"},
		{"pair", "--server", "http://notifier.example", "CODE"},
		{"pair", "--server", "https://x.example", "notifier://pair?server=https://x.example&code=C"},
		{"pair", "notifier://join?server=https://x.example&code=C"},
		{"run", "extra"},
		{"bogus"},
		{},
	} {
		a, _, _ := newTestApp(t)
		if code := a.run(args); code != exitUsage {
			t.Errorf("%v: exit %d", args, code)
		}
	}
}

func TestTestAndUnpair(t *testing.T) {
	api := newFakeAPI(t)
	api.status["POST /v1/device/test"] = http.StatusOK
	api.answer["POST /v1/device/test"] = `{"id":"msg_1","deliveries":[{"device_id":"dev_1","status":"queued"}]}`
	api.status["DELETE /v1/device"] = http.StatusUnauthorized // already unpaired elsewhere
	a, stdout, stderr := newTestApp(t)
	saveState(a.statePath, State{URL: api.URL, DeviceID: "dev_1", DeviceToken: "nd_1", SealKey: testKey, Name: "desk"})

	if code := a.run([]string{"test"}); code != 0 || !strings.Contains(stdout.String(), "Queued") {
		t.Fatalf("test: exit %d, %s %s", code, stdout, stderr)
	}
	if code := a.run([]string{"unpair"}); code != 0 {
		t.Fatalf("unpair: exit %d: %s", code, stderr)
	}
	if _, err := os.Stat(a.statePath); !os.IsNotExist(err) {
		t.Fatal("state file left behind")
	}
	// Exit 4, so systemd does not restart an unpaired daemon.
	if code := a.run([]string{"run"}); code != exitUnpaired || !strings.Contains(stderr.String(), "not paired") {
		t.Fatalf("run unpaired: exit %d: %s", code, stderr)
	}
}

func TestUnpairKeepsStateWhenTheServerFails(t *testing.T) {
	api := newFakeAPI(t)
	api.status["DELETE /v1/device"] = http.StatusServiceUnavailable
	api.answer["DELETE /v1/device"] = `{"error":"could not disconnect the device; try again"}`
	a, _, stderr := newTestApp(t)
	saveState(a.statePath, State{URL: api.URL, DeviceID: "dev_1", DeviceToken: "nd_1", SealKey: testKey})
	if code := a.run([]string{"unpair"}); code != exitError || !strings.Contains(stderr.String(), "try again") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if _, err := os.Stat(a.statePath); err != nil {
		t.Fatal("state removed although the server still has the device")
	}
}

func TestRunExitsWhenUnpaired(t *testing.T) {
	srv := serveStream(t) // rejects every token but testToken
	a, _, stderr := newTestApp(t)
	saveState(a.statePath, State{URL: srv.URL, DeviceID: testDevice, DeviceToken: "nd_gone", SealKey: testKey})
	if code := a.run([]string{"run"}); code != exitUnpaired {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	release, err := lockRun(a.statePath)
	if err != nil {
		t.Fatalf("run kept its lock: %v", err)
	}
	release()
}

func TestRunRefusesASecondDaemon(t *testing.T) {
	a, _, stderr := newTestApp(t)
	saveState(a.statePath, State{URL: "http://127.0.0.1:1", DeviceID: testDevice, DeviceToken: testToken, SealKey: testKey})
	release, err := lockRun(a.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if code := a.run([]string{"run"}); code != exitError || !strings.Contains(stderr.String(), "another notifyd") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	// The same file through a symlink is the same daemon.
	alias := filepath.Join(t.TempDir(), "alias.json")
	if err := os.Symlink(a.statePath, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := lockRun(alias); !errors.Is(err, errAlreadyRunning) {
		t.Fatalf("lock through a symlink: %v", err)
	}
	release()
	if release, err = lockRun(a.statePath); err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	release()
}

func TestBareInvocationPrintsUsage(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("NOTIFYD_STATE", "")
	a := &app{stdout: new(bytes.Buffer), stderr: new(bytes.Buffer)}
	if code := a.run(nil); code != exitUsage || !strings.Contains(a.stderr.(*bytes.Buffer).String(), "Usage:") {
		t.Fatalf("exit %d: %s", code, a.stderr)
	}
}

func TestHelpUnknownCommandsAndOldFlags(t *testing.T) {
	for _, tc := range []struct {
		args []string
		code int
		out  string
	}{
		{[]string{"-h"}, 0, "Available Commands"},
		{[]string{"help", "pair"}, 0, "--server"},
		{[]string{"pair", "--help"}, 0, "--force"},
		{[]string{"listen"}, exitUsage, `unknown command "listen"`},
		{[]string{"history", "-json"}, exitUsage, "--json"},
		{[]string{"completion", "bash"}, 0, "bash completion"},
	} {
		a := &app{stdout: new(bytes.Buffer), stderr: new(bytes.Buffer), statePath: "/nonexistent/s", historyPath: "/nonexistent/h"}
		code := a.run(tc.args)
		out := a.stdout.(*bytes.Buffer).String() + a.stderr.(*bytes.Buffer).String()
		if code != tc.code || !strings.Contains(out, tc.out) {
			t.Errorf("%q: exit %d, output %.300q", tc.args, code, out)
		}
	}
}
