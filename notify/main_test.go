// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

const testKey = "nk_test"

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
	args = append([]string{"-url", f.URL, "-key", testKey}, args...)
	code := run(args, strings.NewReader(stdin), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestSendsToNamedPhone(t *testing.T) {
	f := newFakeServer(t)
	code, stdout, stderr := f.run("", "-to", "pixel", "-high", "Backup done", "2.4 GB")
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
	if code, _, stderr := f.run("", "Hello"); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if len(f.got[0].DeviceIDs) != 0 || f.got[0].Priority != "" {
		t.Fatalf("sent %+v", f.got[0])
	}
}

func TestAcceptsIDsAndCommaSeparatedTargets(t *testing.T) {
	f := newFakeServer(t)
	if code, _, stderr := f.run("", "-to", "dev_tablet,Pixel", "Hi"); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if got := f.got[0].DeviceIDs; len(got) != 2 || got[0] != "dev_tablet" || got[1] != "dev_phone" {
		t.Fatalf("device_ids %v", got)
	}
}

func TestReadsBodyFromStdin(t *testing.T) {
	f := newFakeServer(t)
	f.run("line one\nline two\n", "Log", "-")
	if f.got[0].Body != "line one\nline two" {
		t.Fatalf("body %q", f.got[0].Body)
	}
}

func TestUnknownPhoneFailsWithoutSending(t *testing.T) {
	f := newFakeServer(t)
	code, _, stderr := f.run("", "-to", "watch", "Hi")
	if code != exitError || !strings.Contains(stderr, `"watch"`) || len(f.got) != 0 {
		t.Fatalf("exit %d, stderr %q, sent %d", code, stderr, len(f.got))
	}
}

func TestUndeliveredExitCode(t *testing.T) {
	f := newFakeServer(t)
	f.statuses["dev_phone"] = "unreachable"
	code, _, stderr := f.run("", "-to", "Pixel", "Hi")
	if code != exitUndelivered || !strings.Contains(stderr, "dev_phone: unreachable") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
}

func TestRejectedKey(t *testing.T) {
	f := newFakeServer(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"-url", f.URL, "-key", "nk_wrong", "Hi"}, strings.NewReader(""), &stdout, &stderr)
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
	code := run([]string{"-url", f.URL, "-key", testKey, "-timeout", "200ms", "Build", "-"}, slow, &stdout, &stderr)
	if code != 0 || f.got[0].Body != "partial done" {
		t.Fatalf("exit %d, stderr %q, sent %+v", code, stderr.String(), f.got)
	}
}

func TestLongBodyKeepsTheTail(t *testing.T) {
	f := newFakeServer(t)
	log := strings.Repeat("é", 3000) + "FAILED: last line"
	if code, _, stderr := f.run(log, "Build", "-"); code != 0 {
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
	if code, _, stderr := f.run("", "Build", "-url="+evil.URL); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	f.run("", "Temp", "-5 degrees")
	f.run("", "--", "-high", "-key=x")
	want := []Notification{
		{Title: "Build", Body: "-url=" + evil.URL},
		{Title: "Temp", Body: "-5 degrees"},
		{Title: "-high", Body: "-key=x"},
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
	if code, _, stderr := f.run(latin1, "Log", "-"); code != 0 {
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
	code := run([]string{"-url", f.URL, "-key", testKey, "Big", "-"}, huge, &stdout, &stderr)
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
	f.run("a\r\n", "T", "-")
	if f.got[0].Body != "a" {
		t.Fatalf("body %q", f.got[0].Body)
	}
}

func TestRefusesPlainHTTPExceptLoopback(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"-url", "http://notifier.example.com", "-key", testKey, "Hi"}, strings.NewReader(""), &stdout, &stderr)
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
		code := run([]string{"-url", srv.URL, "-key", testKey, "Hi"}, strings.NewReader(""), &stdout, &stderr)
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
