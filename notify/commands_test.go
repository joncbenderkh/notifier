// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testToken = "nu_test"

type request struct {
	Route, Auth string
	Body        map[string]any
}

// fakeAPI answers each "METHOD /path" with a canned JSON response and
// records the requests it got.
type fakeAPI struct {
	*httptest.Server
	routes map[string]any
	got    []request
}

func newFakeAPI(t *testing.T, routes map[string]any) *fakeAPI {
	t.Helper()
	f := &fakeAPI{routes: routes}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route := r.Method + " " + r.URL.Path
		req := request{Route: route, Auth: r.Header.Get("Authorization")}
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &req.Body)
		f.got = append(f.got, req)
		resp, ok := f.routes[route]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
			return
		}
		if resp == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeAPI) run(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(append([]string{"-url", f.URL}, args...), strings.NewReader(""), &stdout, &stderr, nil)
	return code, stdout.String(), stderr.String()
}

func (f *fakeAPI) routesCalled() []string {
	var routes []string
	for _, r := range f.got {
		routes = append(routes, r.Route)
	}
	return routes
}

// useConfig points the CLI at a config file holding cfg, with mode perm.
func useConfig(t *testing.T, cfg *Config, perm os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "notifier", "config.json")
	t.Setenv("NOTIFIER_CONFIG", path)
	if cfg != nil {
		if err := saveConfig(path, *cfg); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, perm); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

var sent = NotifyResult{ID: "msg_1", Deliveries: []Delivery{{DeviceID: "dev_phone", Status: "sent"}}}

func TestConfigFileSuppliesServerCredentialAndTargets(t *testing.T) {
	f := newFakeAPI(t, map[string]any{"POST /v1/notifications": sent})
	useConfig(t, &Config{URL: f.URL, APIKey: "nk_file", Channels: []string{"ci"}, Priority: "high"}, 0o600)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"Build done"}, strings.NewReader(""), &stdout, &stderr, nil); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	got := f.got[0]
	if got.Auth != "Bearer nk_file" || got.Body["priority"] != "high" || got.Body["channels"].([]any)[0] != "ci" {
		t.Fatalf("got %+v", got)
	}
}

func TestFlagsBeatEnvironmentBeatConfig(t *testing.T) {
	f := newFakeAPI(t, map[string]any{"POST /v1/notifications": sent})
	useConfig(t, &Config{URL: "https://unused.example", APIKey: "nk_file", Channels: []string{"ci"}}, 0o600)
	t.Setenv("NOTIFIER_URL", f.URL)
	t.Setenv("NOTIFIER_API_KEY", "nk_env")

	var stdout, stderr bytes.Buffer
	run([]string{"-c", "alerts", "Hi"}, strings.NewReader(""), &stdout, &stderr, nil)
	run([]string{"-token", "nu_flag", "Hi"}, strings.NewReader(""), &stdout, &stderr, nil)
	if len(f.got) != 2 {
		t.Fatalf("got %d requests: %s", len(f.got), stderr.String())
	}
	// -c replaces the file's channels; -token beats an API key from the
	// environment, because a flag outranks it.
	if f.got[0].Auth != "Bearer nk_env" || f.got[0].Body["channels"].([]any)[0] != "alerts" || f.got[1].Auth != "Bearer nu_flag" {
		t.Fatalf("got %+v", f.got)
	}
}

func TestConfigFileWithCredentialsMustBePrivate(t *testing.T) {
	useConfig(t, &Config{URL: "https://example.com", Token: testToken}, 0o644)
	var stdout, stderr bytes.Buffer
	code := run([]string{"Hi"}, strings.NewReader(""), &stdout, &stderr, nil)
	if code != exitUsage || !strings.Contains(stderr.String(), "chmod 600") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
}

func TestConfigFileRejectsUnknownFields(t *testing.T) {
	path := useConfig(t, nil, 0)
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, []byte(`{"url": "https://example.com", "apikey": "nk_typo"}`), 0o600)
	var stdout, stderr bytes.Buffer
	code := run([]string{"Hi"}, strings.NewReader(""), &stdout, &stderr, nil)
	if code != exitUsage || !strings.Contains(stderr.String(), "apikey") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
}

func TestConfigInitWritesGivenSettingsPrivately(t *testing.T) {
	path := useConfig(t, nil, 0)
	var stdout, stderr bytes.Buffer
	args := []string{"-url", "https://notifier.example", "-token", testToken, "config", "init"}
	if code := run(args, strings.NewReader(""), &stdout, &stderr, nil); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("stat: %v, %v", info, err)
	}
	cfg, err := loadConfig(path)
	if err != nil || cfg.URL != "https://notifier.example" || cfg.Token != testToken || cfg.APIKey != "" {
		t.Fatalf("config %+v, %v", cfg, err)
	}
	if code := run(args, strings.NewReader(""), &stdout, &stderr, nil); code != exitError {
		t.Fatalf("second init: exit %d", code)
	}
}

func TestSignupSavesTheToken(t *testing.T) {
	f := newFakeAPI(t, map[string]any{
		"POST /v1/users": Signup{UserID: "usr_carol", Name: "carol", Token: "nu_carol"},
	})
	path := useConfig(t, &Config{Channels: []string{"ci"}}, 0o600)
	code, stdout, stderr := f.run("signup", "0Z5H-QCEA-3JX4", "carol")
	if code != 0 || !strings.Contains(stdout, "usr_carol") || strings.Contains(stdout+stderr, "nu_carol") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if got := f.got[0]; got.Auth != "" || got.Body["invite"] != "0Z5H-QCEA-3JX4" || got.Body["name"] != "carol" {
		t.Fatalf("got %+v", got)
	}
	cfg, err := loadConfig(path)
	if err != nil || cfg.Token != "nu_carol" || cfg.URL != f.URL || cfg.Channels[0] != "ci" {
		t.Fatalf("config %+v, %v", cfg, err)
	}
	// A second sign-up would lose the first token.
	if code, _, _ := f.run("signup", "AAAA-BBBB-CCCC", "carol2"); code != exitError || len(f.got) != 1 {
		t.Fatalf("second signup: exit %d", code)
	}
}

func TestSendsToChannelsWithUserToken(t *testing.T) {
	f := newFakeAPI(t, map[string]any{"POST /v1/notifications": NotifyResult{
		ID:         "msg_1",
		Deliveries: []Delivery{{DeviceID: "dev_phone", Status: "sent"}},
		Members:    map[string]int{"sent": 2, "unreachable": 1},
	}})
	code, stdout, stderr := f.run("-token", testToken, "-c", "ci,alerts", "-c", "homelab", "Deploy", "done")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	got := f.got[0]
	if got.Auth != "Bearer "+testToken || len(got.Body["channels"].([]any)) != 3 {
		t.Fatalf("got %+v", got)
	}
	if !strings.Contains(stdout, "other members' devices: 2 sent, 1 unreachable") {
		t.Fatalf("stdout %q", stdout)
	}
}

func TestUndeliveredWhenNoMemberDeviceGotIt(t *testing.T) {
	f := newFakeAPI(t, map[string]any{"POST /v1/notifications": NotifyResult{
		ID: "msg_1", Deliveries: []Delivery{}, Members: map[string]int{"failed": 1},
	}})
	if code, _, _ := f.run("-key", testKey, "-c", "shared", "Hi"); code != exitUndelivered {
		t.Fatalf("exit %d", code)
	}
}

func TestCreatesScopedKey(t *testing.T) {
	f := newFakeAPI(t, map[string]any{"POST /v1/keys": APIKey{ID: "key_1", Name: "ci", Key: "nk_new", Channels: []string{"ci"}}})
	code, stdout, stderr := f.run("-token", testToken, "key", "create", "-c", "ci", "ci")
	if code != 0 || strings.TrimSpace(stdout) != "nk_new" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if got := f.got[0]; got.Body["name"] != "ci" || got.Body["channels"].([]any)[0] != "ci" {
		t.Fatalf("got %+v", got)
	}
}

func TestChannelCommandsResolveNames(t *testing.T) {
	expires := time.Now().Add(7 * 24 * time.Hour)
	f := newFakeAPI(t, map[string]any{
		"GET /v1/channels": []Channel{
			{ID: "ch_default_usr_me", Name: "default", Owned: true, Members: 1},
			{ID: "ch_home", Name: "homelab", Owned: true, Members: 2},
			{ID: "ch_alice", Name: "alice/alerts", Members: 3},
		},
		"POST /v1/channels/ch_home/invites": ChannelInvite{
			Code: "ABCD-EFGH-JKMN", Channel: "homelab", ExpiresAt: expires, URI: "notifier://join?server=x&code=ABCD-EFGH-JKMN",
		},
		"GET /v1/channels/ch_home/members": []Member{
			{ID: "usr_me", Name: "me", JoinedAt: expires}, {ID: "usr_bob", Name: "bob", JoinedAt: expires},
		},
		"DELETE /v1/channels/ch_home/members/usr_bob": nil,
		"GET /v1/user": User{ID: "usr_me", Name: "me"},
		"DELETE /v1/channels/ch_alice/members/usr_me": nil,
	})

	code, stdout, stderr := f.run("-token", testToken, "channel", "invite", "homelab")
	if code != 0 || !strings.Contains(stdout, "channel join ABCD-EFGH-JKMN") || !strings.Contains(stdout, "▀") {
		t.Fatalf("invite: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if code, _, stderr := f.run("-token", testToken, "channel", "invite", "alice/alerts"); code != exitError ||
		!strings.Contains(stderr, "not your channel") {
		t.Fatalf("invite to a joined channel: exit %d, %s", code, stderr)
	}
	if code, _, stderr := f.run("-token", testToken, "channel", "remove", "homelab", "bob"); code != 0 {
		t.Fatalf("remove: exit %d, %s", code, stderr)
	}
	if code, _, stderr := f.run("-token", testToken, "channel", "leave", "alice/alerts"); code != 0 {
		t.Fatalf("leave: exit %d, %s", code, stderr)
	}
	if code, _, _ := f.run("-token", testToken, "channel", "delete", "nope"); code != exitError {
		t.Fatalf("unknown channel: exit %d", code)
	}
	for _, want := range []string{
		"POST /v1/channels/ch_home/invites", "DELETE /v1/channels/ch_home/members/usr_bob",
		"DELETE /v1/channels/ch_alice/members/usr_me",
	} {
		if !strings.Contains(strings.Join(f.routesCalled(), "\n"), want) {
			t.Errorf("never called %s; got %v", want, f.routesCalled())
		}
	}
}

func TestSubcommandsNeedTheUserToken(t *testing.T) {
	f := newFakeAPI(t, nil)
	for _, args := range [][]string{{"invite"}, {"channels"}, {"keys"}, {"channel", "create", "ci"}} {
		code, _, stderr := f.run(append([]string{"-key", testKey}, args...)...)
		if code != exitUsage || !strings.Contains(stderr, "NOTIFIER_TOKEN") {
			t.Errorf("%v: exit %d, stderr %q", args, code, stderr)
		}
	}
	if len(f.got) != 0 {
		t.Fatalf("sent %v", f.routesCalled())
	}
}
