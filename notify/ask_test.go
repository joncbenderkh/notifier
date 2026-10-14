// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The shared reply vector, asserted by server/test/seal.test.ts too.
const (
	replyVectorKey        = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
	replyVectorNonce      = "MDEyMzQ1Njc4OWFi"
	replyVectorCiphertext = "TDMzHSVQ7HW1BXHCzRiocGE39XiLrrhG+LEwUrTqrw8="
)

func TestOpensTheSharedReplyVector(t *testing.T) {
	key, _ := base64.StdEncoding.DecodeString(replyVectorKey)
	answer, err := openReply(key, "msg_test", replyVectorNonce, replyVectorCiphertext)
	if err != nil || answer != "yes" {
		t.Fatalf("got %q, %v", answer, err)
	}
	if _, err := openReply(key, "msg_other", replyVectorNonce, replyVectorCiphertext); err == nil {
		t.Fatal("opened a reply sealed for another question")
	}
}

// sealReply seals [answer] as a device does.
func sealReply(t *testing.T, key []byte, id, answer string) (string, string) {
	t.Helper()
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, gcm.NonceSize())
	rand.Read(nonce)
	plaintext, _ := json.Marshal(map[string]string{"answer": answer})
	ct := gcm.Seal(nil, nonce, plaintext, []byte("reply:"+id))
	return base64.StdEncoding.EncodeToString(nonce), base64.StdEncoding.EncodeToString(ct)
}

// askServer answers a question with [answer] (sealed under the key it was
// sent) after [pending] polls, or with the [states] given, in order.
type askServer struct {
	*httptest.Server
	mu       sync.Mutex
	asked    []Notification
	polls    []string
	pending  int
	answer   string
	states   []int // HTTP statuses to answer polls with first
	expired  bool
	statuses map[string]string
	members  map[string]int
	refusal  string // a 404 message to answer the question with
}

func newAskServer(t *testing.T, answer string) *askServer {
	t.Helper()
	s := &askServer{answer: answer, statuses: map[string]string{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/notifications":
			var n Notification
			json.NewDecoder(r.Body).Decode(&n)
			if s.refusal != "" {
				w.WriteHeader(http.StatusNotFound)
				json.NewEncoder(w).Encode(map[string]string{"error": s.refusal})
				return
			}
			s.asked = append(s.asked, n)
			status := s.statuses["dev_phone"]
			if status == "" {
				status = "sent"
			}
			json.NewEncoder(w).Encode(NotifyResult{
				ID:         "msg_q",
				Deliveries: []Delivery{{DeviceID: "dev_phone", Status: status}},
				Members:    s.members,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/replies/msg_q":
			s.polls = append(s.polls, r.URL.RawQuery)
			if len(s.states) > 0 {
				w.WriteHeader(s.states[0])
				s.states = s.states[1:]
				json.NewEncoder(w).Encode(map[string]string{"error": "boom"})
				return
			}
			if s.expired {
				json.NewEncoder(w).Encode(map[string]string{"status": "expired"})
				return
			}
			if s.pending > 0 {
				s.pending--
				json.NewEncoder(w).Encode(map[string]string{"status": "pending"})
				return
			}
			key, _ := base64.StdEncoding.DecodeString(s.asked[len(s.asked)-1].Question.Key)
			n, c := sealReply(t, key, "msg_q", s.answer)
			json.NewEncoder(w).Encode(map[string]string{"status": "answered", "by": "Pixel", "n": n, "c": c})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *askServer) run(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	args = append([]string{"--url", s.URL, "--key", testKey}, args...)
	code := run(args, strings.NewReader(""), &stdout, &stderr, nil)
	return code, stdout.String(), stderr.String()
}

func TestAskPrintsTheAnswer(t *testing.T) {
	s := newAskServer(t, "yes")
	s.pending = 2
	code, stdout, stderr := s.run("ask", "-o", "yes,no", "--wait", "90s", "--high", "Deploy prod?", "v1.2")
	if code != 0 || stdout != "yes\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "dev_phone: sent") || !strings.Contains(stderr, "answered by Pixel") {
		t.Errorf("stderr %q", stderr)
	}
	q := s.asked[0]
	if q.Title != "Deploy prod?" || q.Body != "v1.2" || q.Priority != "high" || q.Question == nil {
		t.Fatalf("asked %+v", q)
	}
	key, err := base64.StdEncoding.DecodeString(q.Question.Key)
	if err != nil || len(key) != replyKeyBytes {
		t.Errorf("key %q", q.Question.Key)
	}
	if strings.Join(q.Question.Options, "|") != "yes|no" || q.Question.ExpiresIn != 90 || q.Question.Anyone {
		t.Errorf("question %+v", q.Question)
	}
	if len(s.polls) != 3 || s.polls[0] != "wait=50" {
		t.Errorf("polls %q", s.polls)
	}
}

func TestAskExitsFourWhenUnanswered(t *testing.T) {
	s := newAskServer(t, "yes")
	s.expired = true
	code, stdout, stderr := s.run("ask", "-o", "ok", "--anyone", "Still there?")
	if code != exitNoAnswer || stdout != "" || !strings.Contains(stderr, "no answer within 10m0s") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if !s.asked[0].Question.Anyone || s.asked[0].Question.ExpiresIn != 600 {
		t.Errorf("question %+v", s.asked[0].Question)
	}
}

func TestAskRetriesServerErrors(t *testing.T) {
	retryPause = time.Millisecond
	t.Cleanup(func() { retryPause = time.Second })
	s := newAskServer(t, "no")
	s.states = []int{http.StatusBadGateway, http.StatusTooManyRequests}
	code, stdout, stderr := s.run("ask", "-o", "yes", "-o", "no", "Deploy?")
	if code != 0 || stdout != "no\n" || strings.Count(stderr, "still waiting") != 1 {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

func TestAskDoesNotReportUnansweredWhenPollsKeepFailing(t *testing.T) {
	retryPause = time.Millisecond
	giveUpAfter = func(time.Duration) time.Duration { return 50 * time.Millisecond }
	t.Cleanup(func() {
		retryPause = time.Second
		giveUpAfter = func(wait time.Duration) time.Duration { return wait + time.Minute }
	})
	s := newAskServer(t, "yes")
	for range 1000 {
		s.states = append(s.states, http.StatusServiceUnavailable)
	}
	code, stdout, stderr := s.run("ask", "-o", "yes", "Deploy?")
	if code != exitError || stdout != "" || !strings.Contains(stderr, "could not get the answer") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

func TestAskStopsOnClientErrors(t *testing.T) {
	s := newAskServer(t, "no")
	s.states = []int{http.StatusNotFound}
	if code, _, stderr := s.run("ask", "-o", "yes", "Deploy?"); code != exitError {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
}

func TestAskRefusesAnAnswerNotOffered(t *testing.T) {
	s := newAskServer(t, "maybe")
	code, stdout, stderr := s.run("ask", "-o", "yes,no", "Deploy?")
	if code != exitError || stdout != "" || !strings.Contains(stderr, `"maybe" is not one of the options`) {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

func TestAskDoesNotWaitWhenNothingWasReached(t *testing.T) {
	s := newAskServer(t, "yes")
	s.statuses["dev_phone"] = "unreachable"
	code, _, stderr := s.run("ask", "-o", "yes", "Deploy?")
	if code != exitUndelivered || len(s.polls) != 0 {
		t.Fatalf("exit %d, polls %q, stderr %q", code, s.polls, stderr)
	}

	// Another member's device counts only when it may answer.
	s.members = map[string]int{"sent": 1}
	if code, _, _ := s.run("ask", "-o", "yes", "Deploy?"); code != exitUndelivered || len(s.polls) != 0 {
		t.Fatalf("exit %d, polls %q", code, s.polls)
	}
	if code, stdout, _ := s.run("ask", "-o", "yes", "--anyone", "Deploy?"); code != 0 || stdout != "yes\n" {
		t.Fatalf("exit %d, stdout %q", code, stdout)
	}
}

func TestAskExitsThreeWhenNoDeviceMayAnswer(t *testing.T) {
	s := newAskServer(t, "yes")
	s.refusal = "no matching paired devices may answer; set anyone to let other members' devices answer"
	code, _, stderr := s.run("ask", "-o", "yes", "-c", "homelab", "Deploy?")
	if code != exitUndelivered || !strings.Contains(stderr, "may answer") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
}

func TestAskChecksItsFlags(t *testing.T) {
	s := newAskServer(t, "yes")
	for _, args := range [][]string{
		{"ask", "Deploy?"},
		{"ask", "-o", ",", "Deploy?"},
		{"ask", "-o", "yes", "--wait", "5s", "Deploy?"},
		{"ask", "-o", "yes", "--wait", "25h", "Deploy?"},
		{"ask", "-o", "yes"},
	} {
		if code, _, _ := s.run(args...); code != exitUsage {
			t.Errorf("%q: exit %d", args, code)
		}
	}
	if len(s.asked) != 0 {
		t.Errorf("asked %+v", s.asked)
	}
}
