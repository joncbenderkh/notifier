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
	"unicode/utf8"
)

const testKey = "nk_test"

// TestMain keeps the developer's own settings out of the tests.
func TestMain(m *testing.M) {
	for _, name := range []string{"NOTIFIER_URL", "NOTIFIER_API_KEY", "NOTIFIER_TOKEN", "NOTIFIER_ADMIN_TOKEN", "XDG_CONFIG_HOME"} {
		os.Unsetenv(name)
	}
	dir, err := os.MkdirTemp("", "notify-test")
	if err != nil {
		panic(err)
	}
	os.Setenv("NOTIFIER_CONFIG", filepath.Join(dir, "absent.json"))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type fakeServer struct {
	*httptest.Server
	got      []Notification
	statuses map[string]string
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	f := &fakeServer{statuses: map[string]string{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testKey {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid API key"})
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/devices":
			json.NewEncoder(w).Encode([]Device{
				{ID: "dev_phone", Name: "Pixel", Reachable: true},
				{ID: "dev_tablet", Name: "Tablet", Reachable: true},
			})
		case "POST /v1/notifications":
			var n Notification
			json.NewDecoder(r.Body).Decode(&n)
			if len(n.Body) > maxBodyBytes {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": "body is too long"})
				return
			}
			f.got = append(f.got, n)
			ids := n.DeviceIDs
			if len(ids) == 0 {
				ids = []string{"dev_phone", "dev_tablet"}
			}
			result := NotifyResult{ID: "msg_1"}
			for _, id := range ids {
				status := f.statuses[id]
				if status == "" {
					status = "sent"
				}
				result.Deliveries = append(result.Deliveries, Delivery{DeviceID: id, Status: status})
			}
			json.NewEncoder(w).Encode(result)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeServer) run(stdin string, args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	args = append([]string{"--url", f.URL, "--key", testKey}, args...)
	code := run(args, strings.NewReader(stdin), &stdout, &stderr, nil)
	return code, stdout.String(), stderr.String()
}

func TestSendsToNamedPhone(t *testing.T) {
	f := newFakeServer(t)
	code, stdout, stderr := f.run("", "send", "--to", "pixel", "--high", "Backup done", "2.4 GB")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	want := Notification{Title: "Backup done", Body: "2.4 GB", Priority: "high", DeviceIDs: []string{"dev_phone"}}
	if len(f.got) != 1 || !equal(f.got[0], want) {
		t.Fatalf("sent %+v, want %+v", f.got, want)
	}
	if stdout != "dev_phone: sent\n" {
		t.Errorf("stdout %q", stdout)
	}
}

func TestSendsToAllPhonesByDefault(t *testing.T) {
	f := newFakeServer(t)
	if code, _, stderr := f.run("", "send", "Hello"); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if len(f.got[0].DeviceIDs) != 0 || f.got[0].Priority != "" {
		t.Fatalf("sent %+v", f.got[0])
	}
}

func TestAcceptsIDsAndCommaSeparatedTargets(t *testing.T) {
	f := newFakeServer(t)
	if code, _, stderr := f.run("", "send", "--to", "dev_tablet,Pixel", "Hi"); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if got := f.got[0].DeviceIDs; len(got) != 2 || got[0] != "dev_tablet" || got[1] != "dev_phone" {
		t.Fatalf("device_ids %v", got)
	}
}

func TestReadsBodyFromStdin(t *testing.T) {
	f := newFakeServer(t)
	f.run("line one\nline two\n", "send", "Log", "-")
	if f.got[0].Body != "line one\nline two" {
		t.Fatalf("body %q", f.got[0].Body)
	}
}

func TestUnknownPhoneFailsWithoutSending(t *testing.T) {
	f := newFakeServer(t)
	code, _, stderr := f.run("", "send", "--to", "watch", "Hi")
	if code != exitUndelivered || !strings.Contains(stderr, `"watch"`) || len(f.got) != 0 {
		t.Fatalf("exit %d, stderr %q, sent %d", code, stderr, len(f.got))
	}
}

func TestUndeliveredExitCode(t *testing.T) {
	f := newFakeServer(t)
	f.statuses["dev_phone"] = "unreachable"
	code, _, stderr := f.run("", "send", "--to", "Pixel", "Hi")
	if code != exitUndelivered || !strings.Contains(stderr, "dev_phone: unreachable") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
}

func TestQueuedCountsAsDelivered(t *testing.T) {
	f := newFakeServer(t)
	f.statuses["dev_phone"] = "queued"
	code, stdout, stderr := f.run("", "send", "--to", "Pixel", "Hi")
	if code != 0 || !strings.Contains(stdout, "dev_phone: queued") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

func TestRejectedKey(t *testing.T) {
	f := newFakeServer(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"send", "--url", f.URL, "--key", "nk_wrong", "Hi"}, strings.NewReader(""), &stdout, &stderr, nil)
	if code != exitError || !strings.Contains(stderr.String(), "rejected the API key") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
}

func TestListsDevices(t *testing.T) {
	f := newFakeServer(t)
	code, stdout, _ := f.run("", "devices")
	if code != 0 || !strings.Contains(stdout, "dev_phone") || !strings.Contains(stdout, "Tablet") {
		t.Fatalf("exit %d, stdout %q", code, stdout)
	}
}

func TestResolveDevicesRejectsAmbiguousNames(t *testing.T) {
	devices := []Device{{ID: "dev_a", Name: "Phone"}, {ID: "dev_b", Name: "phone"}}
	if _, err := ResolveDevices(devices, []string{"PHONE"}); err == nil {
		t.Fatal("ambiguous name resolved")
	}
	if ids, err := ResolveDevices(devices, []string{"dev_b"}); err != nil || ids[0] != "dev_b" {
		t.Fatalf("ids %v, err %v", ids, err)
	}
}

func equal(a, b Notification) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return bytes.Equal(ja, jb)
}

func TestTimeoutStartsAfterStdinIsRead(t *testing.T) {
	f := newFakeServer(t)
	slow := io.MultiReader(strings.NewReader("partial "), &delayedReader{delay: 300 * time.Millisecond, data: "done\n"})
	var stdout, stderr bytes.Buffer
	code := run([]string{"send", "--url", f.URL, "--key", testKey, "--timeout", "200ms", "Build", "-"}, slow, &stdout, &stderr, nil)
	if code != 0 || f.got[0].Body != "partial done" {
		t.Fatalf("exit %d, stderr %q, sent %+v", code, stderr.String(), f.got)
	}
}

func TestLongBodyKeepsTheTail(t *testing.T) {
	f := newFakeServer(t)
	log := strings.Repeat("é", 3000) + "FAILED: last line"
	if code, _, stderr := f.run(log, "send", "Build", "-"); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	body := f.got[0].Body
	if len(body) > maxBodyBytes || !utf8.ValidString(body) || !strings.HasPrefix(body, "…") || !strings.HasSuffix(body, "FAILED: last line") {
		t.Fatalf("body of %d bytes: %q...", len(body), body[:20])
	}
}

func TestMessageTextIsNeverParsedAsFlags(t *testing.T) {
	f := newFakeServer(t)
	evil := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("request reached a server named in the message body")
	}))
	t.Cleanup(evil.Close)
	if code, _, stderr := f.run("", "send", "Build", "--url="+evil.URL); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	f.run("", "send", "Temp", "-5 degrees")
	f.run("", "send", "--", "--high", "--key=x")
	want := []Notification{
		{Title: "Build", Body: "--url=" + evil.URL},
		{Title: "Temp", Body: "-5 degrees"},
		{Title: "--high", Body: "--key=x"},
	}
	for i, w := range want {
		if i >= len(f.got) || !equal(f.got[i], w) {
			t.Fatalf("sent %+v, want %+v", f.got, want)
		}
	}
}

func TestInvalidUTF8StaysWithinLimit(t *testing.T) {
	f := newFakeServer(t)
	latin1 := strings.Repeat("caf\xe9 ", 2000)
	if code, _, stderr := f.run(latin1, "send", "Log", "-"); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	encoded, _ := json.Marshal(f.got[0].Body)
	if len(encoded) > maxBodyBytes+2 || !utf8.ValidString(f.got[0].Body) {
		t.Fatalf("encoded body is %d bytes", len(encoded))
	}
}

func TestHugeStdinIsBoundedAndMarked(t *testing.T) {
	f := newFakeServer(t)
	huge := io.MultiReader(io.LimitReader(xReader{}, 50<<20), strings.NewReader("tail line"))
	var stdout, stderr bytes.Buffer
	code := run([]string{"send", "--url", f.URL, "--key", testKey, "Big", "-"}, huge, &stdout, &stderr, nil)
	body := f.got[0].Body
	if code != 0 || !strings.HasPrefix(body, "…") || !strings.HasSuffix(body, "tail line") || len(body) > maxBodyBytes {
		t.Fatalf("exit %d, body of %d bytes", code, len(body))
	}
}

type xReader struct{}

func (xReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func TestTrimsCRLF(t *testing.T) {
	f := newFakeServer(t)
	f.run("a\r\n", "send", "T", "-")
	if f.got[0].Body != "a" {
		t.Fatalf("body %q", f.got[0].Body)
	}
}

func TestCleansWhatTheServerRefuses(t *testing.T) {
	f := newFakeServer(t)
	log := "\x1b[31mFAIL\x1b[0m pkg\r\n10%\r100%\n\x1b]0;title\x07done\x00 \u202Eevil\tok \u200F\u05E9\n"
	if code, _, stderr := f.run(log, "send", "\x1b[1mBuild\x1b[0m\u2066", "-"); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if got := f.got[0]; got.Title != "Build\uFFFD" || got.Body != "FAIL pkg\n10%\n100%\ndone\uFFFD \uFFFDevil\tok \u200F\u05E9" {
		t.Fatalf("title %q, body %q", got.Title, got.Body)
	}
}

func TestCleanedStdinLosesTrailingNewlines(t *testing.T) {
	f := newFakeServer(t)
	f.run("\x1b(B\x1b[mdone\r\x1b[K", "send", "T", "-")
	if f.got[0].Body != "done" {
		t.Fatalf("body %q", f.got[0].Body)
	}
}

func TestCutTailStartsAtALine(t *testing.T) {
	f := newFakeServer(t)
	log := strings.Repeat("x", stdinTailBytes) + "\x1b[1;31mFAIL\x1b[0m\nlast line\n"
	f.run(log, "send", "T", "-")
	if body := f.got[0].Body; body != "…last line" {
		t.Fatalf("body %q", body)
	}
	// One long line: nothing to cut it at.
	f.run(strings.Repeat("y", stdinTailBytes+10)+"\n", "send", "T", "-")
	if body := f.got[1].Body; !strings.HasSuffix(body, "yyy") {
		t.Fatalf("body %q", body)
	}
}

func TestRefusesPlainHTTPExceptLoopback(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"send", "--url", "http://notifier.example.com", "--key", testKey, "Hi"}, strings.NewReader(""), &stdout, &stderr, nil)
	if code != exitUsage || !strings.Contains(stderr.String(), "https") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	for _, u := range []string{"http://localhost:8787", "http://127.0.0.1:8787", "http://[::1]:8787"} {
		if err := checkURL(u); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
}

func TestNotFoundExitCodes(t *testing.T) {
	for message, want := range map[string]int{noDevicesMessage: exitUndelivered, "not found": exitError} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": message})
		}))
		var stdout, stderr bytes.Buffer
		code := run([]string{"send", "--url", srv.URL, "--key", testKey, "Hi"}, strings.NewReader(""), &stdout, &stderr, nil)
		srv.Close()
		if code != want {
			t.Errorf("%q: exit %d, want %d", message, code, want)
		}
	}
}

type delayedReader struct {
	delay time.Duration
	data  string
	done  bool
}

func (r *delayedReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	time.Sleep(r.delay)
	r.done = true
	return copy(p, r.data), nil
}

func TestRefusesRedirects(t *testing.T) {
	var leaked bool
	plain := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("Authorization") != ""
	}))
	t.Cleanup(plain.Close)
	redirecting := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+r.URL.Path, http.StatusMovedPermanently)
	}))
	t.Cleanup(redirecting.Close)

	var stdout, stderr bytes.Buffer
	args := []string{"send", "--url", redirecting.URL, "--key", testKey, "Hi"}
	code := run(args, strings.NewReader(""), &stdout, &stderr, redirecting.Client().Transport)
	if code != exitError || leaked || !strings.Contains(stderr.String(), "redirected") {
		t.Fatalf("exit %d, leaked %v, stderr %q", code, leaked, stderr.String())
	}
}

func TestPhoneUnpairedBeforeSendIsUndelivered(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode([]Device{{ID: "dev_a", Name: "A"}, {ID: "dev_b", Name: "B"}})
			return
		}
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "no paired device dev_b"})
	}))
	t.Cleanup(srv.Close)
	var stdout, stderr bytes.Buffer
	code := run([]string{"send", "--url", srv.URL, "--key", testKey, "--to", "a,b", "Hi"}, strings.NewReader(""), &stdout, &stderr, nil)
	if code != exitUndelivered || !strings.Contains(stderr.String(), "no paired device dev_b") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
}

func TestCommandNamesAreTitlesOfSend(t *testing.T) {
	f := newFakeServer(t)
	f.run("", "send", "devices")
	f.run("", "send", "--high", "--", "--version")
	if len(f.got) != 2 || f.got[0].Title != "devices" || f.got[1].Title != "--version" {
		t.Fatalf("sent %+v", f.got)
	}
}

func TestOnlySendSends(t *testing.T) {
	f := newFakeServer(t)
	for _, args := range [][]string{{}, {"Hello"}, {"pair", "-h"}, {"devices", "--help"}, {"-high", "send", "x"}, {"send", "-high", "x"}} {
		code, stdout, stderr := f.run("", args...)
		help := len(args) == 2 && strings.HasSuffix(args[1], "h") || len(args) == 2 && args[1] == "--help"
		switch {
		case help && (code != 0 || !strings.Contains(stdout, "Usage:")):
			t.Errorf("%q: exit %d, stdout %q", args, code, stdout)
		case !help && code != exitUsage:
			t.Errorf("%q: exit %d, stderr %q", args, code, stderr)
		}
	}
	if len(f.got) != 0 {
		t.Fatalf("sent %+v", f.got)
	}
	if _, _, stderr := f.run("", "send", "-high", "x"); !strings.Contains(stderr, "--high") {
		t.Errorf("no hint about --high: %q", stderr)
	}
}

func TestCompletionScript(t *testing.T) {
	f := newFakeServer(t)
	code, stdout, _ := f.run("", "completion", "bash")
	if code != 0 || !strings.Contains(stdout, "bash completion") {
		t.Fatalf("exit %d, stdout %.200q", code, stdout)
	}
}

func TestLongTitleIsFitted(t *testing.T) {
	f := newFakeServer(t)
	if code, _, stderr := f.run("", "send", strings.Repeat("é", 200)+"\xff"); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	title := f.got[0].Title
	if len(title) > maxTitleBytes || !utf8.ValidString(title) || !strings.HasSuffix(title, "…") {
		t.Fatalf("title of %d bytes", len(title))
	}
}

func TestPairPrintsQRAndCode(t *testing.T) {
	const token = "nu_test"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/pairings" || r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid user token"})
			return
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(Pairing{Code: "7KQ2-MX9D", ExpiresAt: time.Now().Add(10 * time.Minute), URI: "notifier://pair?server=x&code=7KQ2-MX9D"})
	}))
	t.Cleanup(srv.Close)

	for _, env := range []string{"NOTIFIER_TOKEN", "NOTIFIER_ADMIN_TOKEN"} { // the latter is the old name
		t.Run(env, func(t *testing.T) {
			t.Setenv(env, token)
			var stdout, stderr bytes.Buffer
			code := run([]string{"--url", srv.URL, "pair"}, strings.NewReader(""), &stdout, &stderr, nil)
			if code != 0 || !strings.Contains(stdout.String(), "7KQ2-MX9D") || !strings.Contains(stdout.String(), "▀") {
				t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestPairNeedsUserToken(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--url", "https://example.com", "--key", testKey, "pair"}, strings.NewReader(""), &stdout, &stderr, nil)
	if code != exitUsage || !strings.Contains(stderr.String(), "NOTIFIER_TOKEN") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
}

func TestPairNamesTheUserTokenOn401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid user token"})
	}))
	t.Cleanup(srv.Close)

	var stdout, stderr bytes.Buffer
	code := run([]string{"--url", srv.URL, "--key", testKey, "--token", "nu_wrong", "pair"}, strings.NewReader(""), &stdout, &stderr, nil)
	if code != exitError || !strings.Contains(stderr.String(), "user token") || strings.Contains(stderr.String(), "API key") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
}

func TestPairPrintsCodeWithoutLink(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"code": "7KQ2-MX9D", "expires_at": time.Now().Add(10 * time.Minute)})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("NOTIFIER_TOKEN", "nu_test")

	var stdout, stderr bytes.Buffer
	code := run([]string{"--url", srv.URL, "pair"}, strings.NewReader(""), &stdout, &stderr, nil)
	if code != 0 || !strings.Contains(stdout.String(), "7KQ2-MX9D") || strings.Contains(stdout.String(), "▀") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestRenderQRCoversWholeCode(t *testing.T) {
	out, err := renderQR("notifier://pair?server=https%3A%2F%2Fexample.com&code=7KQ2-MX9D")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	width := strings.Count(lines[0], "▀")
	if width < 21+8 || len(lines) != (width+1)/2 {
		t.Fatalf("%d lines of %d cells", len(lines), width)
	}
}

func TestOldStyleFlagsAreRefusedBeforeParsing(t *testing.T) {
	f := newFakeServer(t)
	for _, args := range [][]string{
		{"send", "-channel=alerts", "T"}, // pflag alone would read -c hannel=alerts
		{"send", "-high", "T"},
		{"-token", "nu_x", "send", "T"},
		{"heartbeat", "create", "-every", "1d", "x"},
		{"-help"},
		{"send", "-help"},
	} {
		if code, _, stderr := f.run("", args...); code != exitUsage || !strings.Contains(stderr, "two dashes") {
			t.Errorf("%q: exit %d, stderr %q", args, code, stderr)
		}
	}
	if len(f.got) != 0 {
		t.Fatalf("sent %+v", f.got)
	}
	// After TITLE, or a flag's value, it is just text.
	for _, args := range [][]string{{"send", "T", "-channel=x"}, {"send", "-c", "a", "--", "-high"}} {
		if code, _, stderr := f.run("", args...); code != 0 {
			t.Errorf("%q: exit %d, %s", args, code, stderr)
		}
	}
	if len(f.got) != 2 || f.got[0].Body != "-channel=x" || f.got[1].Title != "-high" {
		t.Fatalf("sent %+v", f.got)
	}
}

func TestListFlagsSplitCommasButKeepQuotes(t *testing.T) {
	if got := splitList([]string{`Desk "A"`, " a, b ,,c"}); strings.Join(got, "|") != `Desk "A"|a|b|c` {
		t.Fatalf("got %q", got)
	}
}
