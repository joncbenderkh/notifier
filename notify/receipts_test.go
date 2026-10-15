// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// receiptServer takes a message and answers receipt polls with [states]
// in order, then with the last one again.
type receiptServer struct {
	*httptest.Server
	mu     sync.Mutex
	sent   []Notification
	polls  []string
	states []any // a Receipts, or an HTTP status to fail with
	// tracked is how many devices the send says report receipts.
	tracked int
}

func newReceiptServer(t *testing.T, states ...any) *receiptServer {
	t.Helper()
	s := &receiptServer{states: states, tracked: 1}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/notifications":
			var n Notification
			json.NewDecoder(r.Body).Decode(&n)
			s.sent = append(s.sent, n)
			result := NotifyResult{ID: "msg_r", Deliveries: []Delivery{{DeviceID: "dev_phone", Status: "sent"}}}
			if n.Receipts {
				result.Tracked = &s.tracked
			}
			json.NewEncoder(w).Encode(result)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/notifications/msg_r":
			s.polls = append(s.polls, r.URL.RawQuery)
			state := s.states[0]
			if len(s.states) > 1 {
				s.states = s.states[1:]
			}
			if e, ok := state.(*APIError); ok {
				w.WriteHeader(e.Status)
				json.NewEncoder(w).Encode(map[string]string{"error": e.Message})
				return
			}
			if status, ok := state.(int); ok {
				w.WriteHeader(status)
				json.NewEncoder(w).Encode(map[string]string{"error": "boom"})
				return
			}
			json.NewEncoder(w).Encode(state)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *receiptServer) run(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	args = append([]string{"--url", s.URL, "--key", testKey}, args...)
	code := run(args, strings.NewReader(""), &stdout, &stderr, nil)
	return code, stdout.String(), stderr.String()
}

func receipts(status string, devices ...DeviceReceipt) Receipts {
	return Receipts{Status: status, Devices: devices}
}

func TestWaitSeenPrintsWhichDeviceOpenedIt(t *testing.T) {
	s := newReceiptServer(t,
		receipts("pending", DeviceReceipt{DeviceID: "dev_phone", Name: "Pixel"}),
		receipts("opened",
			DeviceReceipt{DeviceID: "dev_laptop", Name: "laptop", ShownAt: 5, OpenedAt: 9},
			DeviceReceipt{DeviceID: "dev_phone", Name: "Pixel", ShownAt: 5, OpenedAt: 7}))
	code, stdout, stderr := s.run("send", "--wait-seen", "Disk full")
	if code != 0 || !strings.Contains(stdout, "dev_phone: sent\nid: msg_r\nopened on Pixel\n") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if !s.sent[0].Receipts || s.sent[0].Title != "Disk full" {
		t.Errorf("sent %+v", s.sent[0])
	}
	if len(s.polls) != 2 || s.polls[0] != "wait=50" {
		t.Errorf("polls %q", s.polls)
	}
}

func TestWaitSeenNamesNoMember(t *testing.T) {
	r := receipts("opened")
	r.Members.Opened = 1
	s := newReceiptServer(t, r)
	if code, stdout, _ := s.run("send", "--wait-seen=1m", "-c", "homelab", "Disk full"); code != 0 || !strings.HasSuffix(stdout, "opened on another member's device\n") {
		t.Fatalf("exit %d, stdout %q", code, stdout)
	}
}

func TestWaitSeenExitsFourWhenNobodyOpensIt(t *testing.T) {
	// Each reading of the clock is 4 s after the one before.
	start, reads := time.Now(), 0
	seenClock = func() time.Time { reads++; return start.Add(time.Duration(reads-1) * 4 * time.Second) }
	t.Cleanup(func() { seenClock = time.Now })
	s := newReceiptServer(t, receipts("pending"))
	code, _, stderr := s.run("send", "--wait-seen=10s", "Disk full")
	if code != exitNotSeen || !strings.Contains(stderr, "not opened within 10s") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if strings.Join(s.polls, " ") != "wait=6 wait=0" {
		t.Errorf("polls %q", s.polls)
	}
}

func TestWaitSeenRetriesServerErrors(t *testing.T) {
	retryPause = time.Millisecond
	t.Cleanup(func() { retryPause = time.Second })
	s := newReceiptServer(t, http.StatusBadGateway, receipts("opened", DeviceReceipt{Name: "Pixel", OpenedAt: 1}))
	code, stdout, stderr := s.run("send", "--wait-seen", "Disk full")
	if code != 0 || !strings.Contains(stdout, "opened on Pixel") || strings.Count(stderr, "still waiting") != 1 {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

func TestWaitSeenChecksTheDuration(t *testing.T) {
	s := newReceiptServer(t, receipts("pending"))
	for _, d := range []string{"0s", "5s", "25h"} {
		if code, _, stderr := s.run("send", "--wait-seen="+d, "x"); code != exitUsage || !strings.Contains(stderr, "--wait-seen must be") {
			t.Errorf("%s: exit %d, stderr %q", d, code, stderr)
		}
	}
	if len(s.sent) != 0 {
		t.Errorf("sent %+v", s.sent)
	}
}

func TestWaitSeenIgnoresAnOpeningPastTheDeadline(t *testing.T) {
	retryPause = time.Millisecond
	start, reads := time.Now(), 0
	// The deadline is read first; every poll is asked after it.
	seenClock = func() time.Time { reads++; return start.Add(time.Duration(min(reads-1, 1)) * 20 * time.Second) }
	t.Cleanup(func() { retryPause, seenClock = time.Second, time.Now })
	for _, tc := range []struct {
		openedAfter time.Duration
		code        int
	}{{15 * time.Second, exitNotSeen}, {5 * time.Second, 0}} {
		reads = 0
		late := receipts("opened", DeviceReceipt{Name: "Pixel", OpenedAt: start.Add(tc.openedAfter).Unix()})
		s := newReceiptServer(t, http.StatusBadGateway, late)
		if code, stdout, stderr := s.run("send", "--wait-seen=10s", "Disk full"); code != tc.code {
			t.Errorf("opened after %s: exit %d, stdout %q, stderr %q", tc.openedAfter, code, stdout, stderr)
		}
	}
	members := receipts("opened")
	members.Members.Opened = 1
	reads = 0
	s := newReceiptServer(t, http.StatusBadGateway, members)
	if code, _, stderr := s.run("send", "--wait-seen=10s", "Disk full"); code != exitError || !strings.Contains(stderr, "perhaps not within 10s") {
		t.Errorf("a member's untimed opening seen after the deadline: exit %d, stderr %q", code, stderr)
	}
}

func TestWaitSeenIgnoresAnOpeningPastTheDeadlineInALateAnswer(t *testing.T) {
	start, reads := time.Now(), 0
	// The deadline and the poll are read in time; the answer comes after the deadline.
	seenClock = func() time.Time { reads++; return start.Add(time.Duration(max(reads-2, 0)) * 15 * time.Second) }
	t.Cleanup(func() { seenClock = time.Now })
	own := receipts("opened", DeviceReceipt{Name: "Pixel", OpenedAt: start.Add(14 * time.Second).Unix()})
	s := newReceiptServer(t, own)
	if code, stdout, stderr := s.run("send", "--wait-seen=10s", "Disk full"); code != exitNotSeen {
		t.Errorf("own late opening: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	members := receipts("opened")
	members.Members.Opened = 1
	reads = 0
	s = newReceiptServer(t, members)
	if code, _, stderr := s.run("send", "--wait-seen=10s", "Disk full"); code != exitError || !strings.Contains(stderr, "perhaps not within 10s") {
		t.Errorf("member opening in a late answer: exit %d, stderr %q", code, stderr)
	}
}

func TestWaitSeenRefusesASpacedDuration(t *testing.T) {
	s := newReceiptServer(t, receipts("opened", DeviceReceipt{Name: "Pixel", OpenedAt: 1}))
	for _, d := range []string{"5m", "300", "5min", "1.5 h"} {
		if code, _, stderr := s.run("send", "--wait-seen", d, "Disk full"); code != exitUsage || !strings.Contains(stderr, "as in --wait-seen=5m") {
			t.Errorf("%s: exit %d, stderr %q", d, code, stderr)
		}
	}
	if len(s.sent) != 0 {
		t.Fatalf("sent %+v", s.sent)
	}
	if code, _, stderr := s.run("send", "--wait-seen", "--", "5m", "Disk full"); code != 0 || s.sent[0].Title != "5m" {
		t.Errorf("a title after --: exit %d, stderr %q, sent %+v", code, stderr, s.sent)
	}
	// A title that starts with a number is no duration.
	if code, _, stderr := s.run("send", "--wait-seen", "3 failed", "in build"); code != 0 || s.sent[1].Title != "3 failed" {
		t.Errorf("a numbered title: exit %d, stderr %q, sent %+v", code, stderr, s.sent)
	}
	// Given with =, even the default is a duration, and what follows the title.
	if code, _, stderr := s.run("send", "--wait-seen=10m", "300", "errors"); code != 0 || s.sent[2].Title != "300" {
		t.Errorf("an explicit duration: exit %d, stderr %q, sent %+v", code, stderr, s.sent)
	}
	// Another flag in between: the word can only be the title.
	if code, _, stderr := s.run("send", "--wait-seen", "--high", "5m", "errors"); code != 0 || s.sent[3].Title != "5m" {
		t.Errorf("a flag in between: exit %d, stderr %q, sent %+v", code, stderr, s.sent)
	}
}

func TestBareWaitSeenKeepsAnEarlierDuration(t *testing.T) {
	var w seenWait
	w.Set("1m")
	w.Set(bareSeenWait)
	if w.d != time.Minute {
		t.Fatalf("got %s", w.d)
	}
}

func TestWaitSeenStopsWhenNoDeviceReports(t *testing.T) {
	s := newReceiptServer(t, receipts("pending"))
	s.tracked = 0
	code, stdout, stderr := s.run("send", "--wait-seen=10s", "Disk full")
	if code != exitError || !strings.Contains(stderr, "no device reports receipts") || len(s.polls) != 0 || !strings.HasSuffix(stdout, "id: msg_r\n") {
		t.Fatalf("exit %d, stdout %q, stderr %q, polls %q", code, stdout, stderr, s.polls)
	}
	// Without waiting it only warns.
	if code, _, stderr := s.run("send", "--receipts", "Disk full"); code != 0 || !strings.Contains(stderr, "no device reports receipts") {
		t.Fatalf("--receipts: exit %d, stderr %q", code, stderr)
	}
}

func TestWaitSeenCountsAnOpeningStampedLateInATimelyAnswer(t *testing.T) {
	// The server's clock runs ahead: it stamps an opening after the
	// client's deadline, in an answer that comes before it.
	start := time.Now()
	seenClock = func() time.Time { return start }
	t.Cleanup(func() { seenClock = time.Now })
	s := newReceiptServer(t, receipts("opened", DeviceReceipt{Name: "Pixel", OpenedAt: start.Add(time.Minute).Unix()}))
	if code, stdout, _ := s.run("send", "--wait-seen=10s", "Disk full"); code != 0 || !strings.Contains(stdout, "opened on Pixel") || len(s.polls) != 1 {
		t.Errorf("exit %d, stdout %q, polls %q", code, stdout, s.polls)
	}
}

func TestReceiptsPrintsTheID(t *testing.T) {
	s := newReceiptServer(t, receipts("pending"))
	code, stdout, _ := s.run("send", "--receipts", "Disk full")
	if code != 0 || !strings.HasSuffix(stdout, "id: msg_r\n") || !s.sent[0].Receipts || len(s.polls) != 0 {
		t.Fatalf("exit %d, stdout %q, polls %q", code, stdout, s.polls)
	}
	s.run("send", "Plain")
	if s.sent[1].Receipts {
		t.Error("an ordinary send asked for receipts")
	}
}

func TestStatusPrintsEachDevice(t *testing.T) {
	r := receipts("opened",
		DeviceReceipt{Name: "Pixel", ShownAt: 1_790_000_000, OpenedAt: 1_790_000_060},
		DeviceReceipt{Name: "laptop", ShownAt: 1_790_000_000},
		DeviceReceipt{Name: "tablet"})
	r.Members.Shown = 2
	s := newReceiptServer(t, r)
	code, stdout, stderr := s.run("status", "msg_r")
	want := "Pixel: shown " + stamp(1_790_000_000) + ", opened " + stamp(1_790_000_060) + "\n" +
		"laptop: shown " + stamp(1_790_000_000) + "\n" +
		"tablet: not shown\n" +
		"other members' devices: 2 shown, 0 opened\n"
	if code != 0 || stdout != want {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if s.polls[0] != "wait=0" {
		t.Errorf("polls %q", s.polls)
	}
}

func TestStatusSaysWhenNoDeviceReports(t *testing.T) {
	s := newReceiptServer(t, receipts("pending"))
	if code, stdout, _ := s.run("status", "msg_r"); code != 0 || !strings.HasPrefix(stdout, "no device reports receipts") {
		t.Fatalf("exit %d, stdout %q", code, stdout)
	}
	r := receipts("pending")
	r.Members.Tracked = 2
	s = newReceiptServer(t, r)
	if code, stdout, _ := s.run("status", "msg_r"); code != 0 || stdout != "not shown on any device yet\n" {
		t.Fatalf("exit %d, stdout %q", code, stdout)
	}
}

func TestStatusExplainsAnUntrackedMessage(t *testing.T) {
	s := newReceiptServer(t, &APIError{http.StatusNotFound, noReceipts})
	if code, _, stderr := s.run("status", "msg_r"); code != exitError || !strings.Contains(stderr, "sent without --receipts") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	// Another 404, such as a missing route on the wrong server, is not explained away.
	s = newReceiptServer(t, &APIError{http.StatusNotFound, "not found"})
	if code, _, stderr := s.run("status", "msg_r"); code != exitError || strings.Contains(stderr, "sent without --receipts") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
}
