// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
