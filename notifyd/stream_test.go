// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const (
	testDevice = "dev_desk"
	testToken  = "nd_test"
	testKey    = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
)

type fakeDesktop struct {
	shown chan Payload
	fail  atomic.Int32 // fail this many calls first
	// onShow, if set, runs as a message is shown.
	onShow func()
}

func newFakeDesktop() *fakeDesktop { return &fakeDesktop{shown: make(chan Payload, 10)} }

func (d *fakeDesktop) Notify(_ context.Context, p Payload) error {
	if d.fail.Add(-1) >= 0 {
		return errors.New("no notification daemon")
	}
	if p.Title == "poison" {
		return errRejected
	}
	if d.onShow != nil {
		d.onShow()
	}
	d.shown <- p
	return nil
}

func (d *fakeDesktop) next(t *testing.T) Payload {
	t.Helper()
	select {
	case p := <-d.shown:
		return p
	case <-time.After(5 * time.Second):
		t.Fatal("nothing shown")
		return Payload{}
	}
}

func (d *fakeDesktop) none(t *testing.T) {
	t.Helper()
	select {
	case p := <-d.shown:
		t.Fatalf("unexpectedly shown: %+v", p)
	default:
	}
}

// sealFrame builds the frame the server sends for p, sealed as envelope v1.
func sealFrame(t *testing.T, p Payload) []byte {
	t.Helper()
	key, _ := base64.StdEncoding.DecodeString(testKey)
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, gcm.NonceSize())
	rand.Read(nonce)
	plaintext, _ := json.Marshal(p)
	f, _ := json.Marshal(frame{
		Type: "message", ID: p.ID, V: envelopeVersion,
		N: base64.StdEncoding.EncodeToString(nonce),
		C: base64.StdEncoding.EncodeToString(gcm.Seal(nil, nonce, plaintext, []byte(testDevice))),
	})
	return f
}

// conn is the server side of one test connection.
type conn struct {
	t  *testing.T
	ws *websocket.Conn
}

func (c conn) send(data []byte) {
	if err := c.ws.Write(context.Background(), websocket.MessageText, data); err != nil {
		c.t.Errorf("send: %v", err)
	}
}

// ack reads frames until the next ack, answering pings, and returns its ID.
func (c conn) ack() string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, data, err := c.ws.Read(ctx)
		if err != nil {
			c.t.Errorf("waiting for an ack: %v", err)
			return ""
		}
		if string(data) == "ping" {
			c.send([]byte("pong"))
			continue
		}
		var a struct{ Ack string }
		json.Unmarshal(data, &a)
		return a.Ack
	}
}

// serveStream runs each connection through the next scenario in turn; extra
// connections idle until the client closes them.
func serveStream(t *testing.T, scenarios ...func(conn)) *httptest.Server {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/device/stream" || r.Header.Get("Authorization") != "Bearer "+testToken {
			http.Error(w, `{"error":"invalid device token"}`, http.StatusUnauthorized)
			return
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer ws.CloseNow()
		i := int(n.Add(1)) - 1
		if i >= len(scenarios) {
			// Reading answers the client's close handshake.
			for {
				if _, _, err := ws.Read(r.Context()); err != nil {
					return
				}
			}
		}
		scenarios[i](conn{t, ws})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testStreamer(server string, d Desktop, token string) *streamer {
	s := newStreamer(State{URL: server, DeviceID: testDevice, DeviceToken: token, SealKey: testKey}, d, http.DefaultClient, func(string, ...any) {})
	s.minBackoff, s.maxBackoff = 10*time.Millisecond, 50*time.Millisecond
	s.pingInterval, s.readTimeout = 20*time.Millisecond, time.Second
	return s
}

func runStream(t *testing.T, s *streamer) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return s.run(ctx)
}

func TestShowsAcksAndDropsReplays(t *testing.T) {
	desk := newFakeDesktop()
	srv := serveStream(t, func(c conn) {
		c.send(sealFrame(t, Payload{ID: "msg_1", Title: "Disk full", Priority: "high", Channel: "homelab"}))
		if got := c.ack(); got != "msg_1" {
			t.Errorf("ack %q", got)
		}
		// A replay the server sends because it missed the ack: acked, not shown.
		c.send(sealFrame(t, Payload{ID: "msg_1", Title: "Disk full", Priority: "high"}))
		if got := c.ack(); got != "msg_1" {
			t.Errorf("replay ack %q", got)
		}
		c.ws.Close(closeUnpaired, "device unpaired")
	})
	if err := runStream(t, testStreamer(srv.URL, desk, testToken)); !errors.Is(err, errUnpaired) {
		t.Fatalf("run: %v", err)
	}
	if p := desk.next(t); p.Title != "Disk full" || p.Channel != "homelab" {
		t.Fatalf("shown %+v", p)
	}
	desk.none(t)
}

func TestReconnectsAfterADrop(t *testing.T) {
	desk := newFakeDesktop()
	srv := serveStream(t,
		func(c conn) { c.ws.CloseNow() },
		func(c conn) {
			// Silent past the read timeout: the client must give up on it.
			time.Sleep(1500 * time.Millisecond)
		},
		func(c conn) {
			c.send(sealFrame(t, Payload{ID: "msg_2", Title: "Back"}))
			c.ack()
			c.ws.Close(closeReplaced, "replaced by a newer connection")
		},
	)
	s := testStreamer(srv.URL, desk, testToken)
	s.pingInterval = time.Hour
	if err := runStream(t, s); !errors.Is(err, errReplaced) {
		t.Fatalf("run: %v", err)
	}
	if p := desk.next(t); p.Title != "Back" {
		t.Fatalf("shown %+v", p)
	}
}

func TestKeepsUnshownMessagesForTheNextConnection(t *testing.T) {
	desk := newFakeDesktop()
	desk.fail.Store(1)
	msg := Payload{ID: "msg_3", Title: "Retry me"}
	srv := serveStream(t,
		func(c conn) {
			c.send(sealFrame(t, msg))
			// The desktop failed, so the client hangs up without acking.
			if _, _, err := c.ws.Read(context.Background()); err == nil {
				t.Error("acked a message that was not shown")
			}
		},
		func(c conn) {
			c.send(sealFrame(t, msg))
			if got := c.ack(); got != "msg_3" {
				t.Errorf("ack %q", got)
			}
			c.ws.Close(closeUnpaired, "")
		},
	)
	if err := runStream(t, testStreamer(srv.URL, desk, testToken)); !errors.Is(err, errUnpaired) {
		t.Fatalf("run: %v", err)
	}
	if p := desk.next(t); p.Title != "Retry me" {
		t.Fatalf("shown %+v", p)
	}
}

func TestDropsMessagesItCannotOpen(t *testing.T) {
	desk := newFakeDesktop()
	srv := serveStream(t, func(c conn) {
		c.send([]byte(`{"type":"message","id":"msg_bad","v":"1","n":"AAAAAAAAAAAAAAAA","c":"AAAA"}`))
		if got := c.ack(); got != "msg_bad" {
			t.Errorf("ack %q", got)
		}
		f := sealFrame(t, Payload{ID: "msg_real"})
		var swapped frame
		json.Unmarshal(f, &swapped)
		swapped.ID = "msg_other" // sealed under another ID
		f, _ = json.Marshal(swapped)
		c.send(f)
		if got := c.ack(); got != "msg_other" {
			t.Errorf("ack %q", got)
		}
		c.send([]byte(`{"type":"hello"}`))
		c.ws.Close(closeUnpaired, "")
	})
	if err := runStream(t, testStreamer(srv.URL, desk, testToken)); !errors.Is(err, errUnpaired) {
		t.Fatalf("run: %v", err)
	}
	desk.none(t)
}

func TestDropsWhatTheDesktopRejectsWithoutHoldingUpTheRest(t *testing.T) {
	desk := newFakeDesktop()
	srv := serveStream(t, func(c conn) {
		c.send(sealFrame(t, Payload{ID: "msg_poison", Title: "poison"}))
		if got := c.ack(); got != "msg_poison" {
			t.Errorf("ack %q", got)
		}
		c.send(sealFrame(t, Payload{ID: "msg_next", Title: "next"}))
		c.ack()
		c.ws.Close(closeUnpaired, "")
	})
	if err := runStream(t, testStreamer(srv.URL, desk, testToken)); !errors.Is(err, errUnpaired) {
		t.Fatalf("run: %v", err)
	}
	if p := desk.next(t); p.Title != "next" {
		t.Fatalf("shown %+v", p)
	}
}

func TestLeavesNewerEnvelopesQueued(t *testing.T) {
	desk := newFakeDesktop()
	srv := serveStream(t, func(c conn) {
		c.send([]byte(`{"type":"message","id":"msg_v2","v":"2","n":"","c":""}`))
		c.send(sealFrame(t, Payload{ID: "msg_v1", Title: "v1"}))
		if got := c.ack(); got != "msg_v1" {
			t.Errorf("first ack %q, want msg_v1 (msg_v2 must stay queued)", got)
		}
		c.ws.Close(closeUnpaired, "")
	})
	if err := runStream(t, testStreamer(srv.URL, desk, testToken)); !errors.Is(err, errUnpaired) {
		t.Fatalf("run: %v", err)
	}
	desk.next(t)
}

func TestStopsWhenTheServerRejectsTheDevice(t *testing.T) {
	srv := serveStream(t)
	if err := runStream(t, testStreamer(srv.URL, newFakeDesktop(), "nd_revoked")); !errors.Is(err, errUnpaired) {
		t.Fatalf("run: %v", err)
	}
}

func TestStopsCleanlyWhenCancelled(t *testing.T) {
	srv := serveStream(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- testStreamer(srv.URL, newFakeDesktop(), testToken).run(ctx) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("did not stop")
	}
}

func TestGivesUpOnAStalledUpgrade(t *testing.T) {
	stalled := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-stalled:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(stalled); srv.Close() })
	s := testStreamer(srv.URL, newFakeDesktop(), testToken)
	s.dialTimeout = 50 * time.Millisecond
	start := time.Now()
	if _, err := s.session(context.Background()); err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("session: %v after %s", err, time.Since(start))
	}
}

func TestRecentIDsForgetsTheOldest(t *testing.T) {
	r := newRecentIDs(2)
	r.add("a")
	r.add("b")
	r.add("a")
	r.add("c")
	if r.has("a") || !r.has("b") || !r.has("c") {
		t.Fatalf("%v", r.order)
	}
}

func TestStreamURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://notifier.example":   "wss://notifier.example/v1/device/stream",
		"http://127.0.0.1:8787":      "ws://127.0.0.1:8787/v1/device/stream",
		"https://notifier.example:8": "wss://notifier.example:8/v1/device/stream",
	} {
		if got := streamURL(in); got != want {
			t.Errorf("streamURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestShowsUnderTheReminderLock(t *testing.T) {
	desktop := newFakeDesktop()
	s := newStreamer(State{}, desktop, http.DefaultClient, t.Logf)
	var mu sync.Mutex
	s.showing = &mu
	mu.Lock()
	done := make(chan error, 1)
	go func() { done <- s.show(context.Background(), Payload{ID: "msg_1", Title: "Disk 95%", Key: "disk"}) }()
	select {
	case p := <-desktop.shown:
		t.Fatalf("shown while a reminder showed: %+v", p)
	case <-time.After(50 * time.Millisecond):
	}
	mu.Unlock()
	desktop.next(t)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
